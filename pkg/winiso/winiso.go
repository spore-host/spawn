// Package winiso inspects a Windows installation ISO to determine which
// editions it contains and whether it is acceptable to AWS EC2 Image Builder's
// import-disk-image workflow — without mounting the ISO or shelling out to any
// platform tool. It works by locating the WIM XML metadata block (which every
// install.wim embeds as a UTF-16LE <WIM>…</WIM> document listing each edition,
// its image index, EDITIONID, architecture and build) directly in the byte
// stream and parsing it.
package winiso

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf16"
)

// Edition is one Windows image inside the ISO's install.wim.
type Edition struct {
	Index     int    // 1-based image index (pass as --image-index)
	Name      string // e.g. "Windows 11 Enterprise"
	EditionID string // e.g. "Enterprise"
	Arch      string // "x64", "arm64", "x86", or a raw code
	Build     string // e.g. "26200"
	Eval      bool   // an Evaluation image
	Supported bool   // documented as supported by import-disk-image
	Note      string // caveat shown to the user
}

// Report is the verdict for an ISO.
type Report struct {
	Editions      []Edition
	HasEnterprise bool // a non-Eval Enterprise/Enterprise N x64 edition is present
	AnyEval       bool // any edition is an Evaluation image
	LooksConsumer bool // no Enterprise at all and Home present (consumer media)
	Acceptable    bool // at least one edition import-disk-image accepts
	// RecommendedIndex is the image index to pass to `spawn image import`
	// (the first non-Eval Enterprise x64 edition), or 0 if none.
	RecommendedIndex int
	Summary          string
}

// wimDoc mirrors the relevant parts of the WIM XML metadata document.
type wimDoc struct {
	Images []wimImage `xml:"IMAGE"`
}

type wimImage struct {
	Index   int    `xml:"INDEX,attr"`
	Name    string `xml:"NAME"`
	Flags   string `xml:"FLAGS"`
	Windows struct {
		Arch             int    `xml:"ARCH"`
		EditionID        string `xml:"EDITIONID"`
		InstallationType string `xml:"INSTALLATIONTYPE"`
		Version          struct {
			Build string `xml:"BUILD"`
		} `xml:"VERSION"`
	} `xml:"WINDOWS"`
}

var (
	wimStart = utf16le("<WIM>")
	wimEnd   = utf16le("</WIM>")
)

// InspectFile scans an ISO file and returns its edition report. To stay fast on
// multi-GB media it scans the tail of the file first (install.wim's metadata
// lives near the end), falling back to a full scan only if nothing is found.
func InspectFile(path string) (*Report, error) {
	f, err := os.Open(path) //nolint:gosec // explicit user-supplied ISO path
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}

	const tail = 768 << 20 // 768 MiB
	if fi.Size() > tail {
		if _, err := f.Seek(fi.Size()-tail, io.SeekStart); err == nil {
			if blocks, _ := scanWimBlocks(f); len(blocks) > 0 {
				if rep := buildReport(blocks); rep != nil && len(rep.Editions) > 0 {
					return rep, nil
				}
			}
		}
		// Fall back to a full scan from the start.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}

	blocks, err := scanWimBlocks(f)
	if err != nil && err != io.EOF {
		return nil, err
	}
	rep := buildReport(blocks)
	if rep == nil || len(rep.Editions) == 0 {
		return nil, fmt.Errorf("no Windows install image (install.wim) metadata found in %s — is this a Windows installation ISO?", path)
	}
	return rep, nil
}

// Inspect scans an arbitrary reader (used in tests). It reads to EOF.
func Inspect(r io.Reader) (*Report, error) {
	blocks, err := scanWimBlocks(r)
	if err != nil && err != io.EOF {
		return nil, err
	}
	rep := buildReport(blocks)
	if rep == nil || len(rep.Editions) == 0 {
		return nil, fmt.Errorf("no WIM metadata found")
	}
	return rep, nil
}

// scanWimBlocks streams r and returns each UTF-16LE <WIM>…</WIM> blob it finds.
func scanWimBlocks(r io.Reader) ([][]byte, error) {
	const chunk = 8 << 20
	const maxBlock = 16 << 20 // a WIM XML doc is tens of KB; cap defensively
	br := bufio.NewReaderSize(r, chunk)
	buf := make([]byte, chunk)

	var blocks [][]byte
	var cur []byte
	var pending []byte
	capturing := false

	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			data := append(pending, buf[:n]...)
			pending = nil
			for len(data) > 0 {
				if !capturing {
					idx := bytes.Index(data, wimStart)
					if idx < 0 {
						if keep := len(wimStart) - 1; len(data) > keep {
							pending = append(pending, data[len(data)-keep:]...)
						} else {
							pending = append(pending, data...)
						}
						break
					}
					capturing = true
					cur = cur[:0]
					data = data[idx:]
					continue
				}
				// capturing: look for the end marker
				idx := bytes.Index(data, wimEnd)
				if idx < 0 {
					keep := len(wimEnd) - 1
					if len(data) > keep {
						cur = append(cur, data[:len(data)-keep]...)
						pending = append(pending, data[len(data)-keep:]...)
					} else {
						pending = append(pending, data...)
					}
					if len(cur) > maxBlock { // runaway: abandon this block
						capturing = false
						cur = cur[:0]
					}
					break
				}
				end := idx + len(wimEnd)
				cur = append(cur, data[:end]...)
				block := make([]byte, len(cur))
				copy(block, cur)
				blocks = append(blocks, block)
				capturing = false
				cur = cur[:0]
				data = data[end:]
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return blocks, nil
			}
			return blocks, rerr
		}
	}
}

// buildReport parses the WIM blocks, keeps the client-install images (boot.wim
// images are WindowsPE and ignored), and computes the verdict.
func buildReport(blocks [][]byte) *Report {
	// The WIM XML metadata can appear more than once across the byte stream
	// (e.g. in both install.wim and a tail rescan). Dedupe by image index,
	// preferring the richest record (one with an EDITIONID).
	byIndex := map[int]wimImage{}
	for _, b := range blocks {
		var doc wimDoc
		if err := xml.Unmarshal([]byte(decodeUTF16LE(b)), &doc); err != nil {
			continue
		}
		for _, im := range doc.Images {
			// install.wim images are "Client"; boot.wim images are "WindowsPE".
			if strings.EqualFold(im.Windows.InstallationType, "WindowsPE") {
				continue
			}
			if im.Name == "" && im.Windows.EditionID == "" {
				continue
			}
			prev, ok := byIndex[im.Index]
			if !ok || (prev.Windows.EditionID == "" && im.Windows.EditionID != "") {
				byIndex[im.Index] = im
			}
		}
	}
	if len(byIndex) == 0 {
		return nil
	}
	images := make([]wimImage, 0, len(byIndex))
	for _, im := range byIndex {
		images = append(images, im)
	}

	rep := &Report{}
	hasHome := false
	for _, im := range images {
		e := Edition{
			Index:     im.Index,
			Name:      strings.TrimSpace(im.Name),
			EditionID: strings.TrimSpace(im.Windows.EditionID),
			Arch:      archName(im.Windows.Arch),
			Build:     strings.TrimSpace(im.Windows.Version.Build),
		}
		e.Eval = isEval(e.Name, e.EditionID)
		classifyEdition(&e)

		if e.Eval {
			rep.AnyEval = true
		}
		if strings.Contains(strings.ToLower(e.EditionID), "core") ||
			strings.HasPrefix(strings.ToLower(e.Name), "windows 11 home") {
			hasHome = true
		}
		rep.Editions = append(rep.Editions, e)
	}

	sort.Slice(rep.Editions, func(i, j int) bool { return rep.Editions[i].Index < rep.Editions[j].Index })

	// Recommend deterministically: prefer plain "Enterprise" over "Enterprise N",
	// then the lowest index among supported editions.
	for _, e := range rep.Editions {
		if e.Supported && strings.EqualFold(e.EditionID, "Enterprise") {
			rep.RecommendedIndex = e.Index
			rep.HasEnterprise = true
			break
		}
	}
	if rep.RecommendedIndex == 0 {
		for _, e := range rep.Editions {
			if e.Supported {
				rep.RecommendedIndex = e.Index
				rep.HasEnterprise = true
				break
			}
		}
	}

	rep.LooksConsumer = !rep.HasEnterprise && hasHome
	rep.Acceptable = rep.HasEnterprise
	rep.Summary = summarize(rep)
	return rep
}

// classifyEdition sets Supported + Note per the import-disk-image docs:
// supported = Windows 11 Enterprise / Enterprise N, x64, non-Evaluation.
func classifyEdition(e *Edition) {
	id := strings.ToLower(e.EditionID)
	isEnt := id == "enterprise" || id == "enterprisen"
	switch {
	case e.Eval:
		e.Supported = false
		e.Note = "Evaluation image — rejected by import-disk-image"
	case isEnt && e.Arch == "x64":
		e.Supported = true
		e.Note = "supported"
	case isEnt && e.Arch != "x64":
		e.Supported = false
		e.Note = "Enterprise but not x64 — import-disk-image is x64-only"
	default:
		e.Supported = false
		e.Note = "not in the documented set (Enterprise x64 only); requires Enterprise licensing to run regardless"
	}
}

func isEval(name, editionID string) bool {
	return strings.Contains(strings.ToLower(name), "evaluation") ||
		strings.HasSuffix(strings.ToLower(editionID), "eval")
}

func archName(code int) string {
	switch code {
	case 0:
		return "x86"
	case 9:
		return "x64"
	case 12:
		return "arm64"
	case 5:
		return "arm"
	case 6:
		return "ia64"
	default:
		return fmt.Sprintf("arch%d", code)
	}
}

func summarize(rep *Report) string {
	switch {
	case rep.HasEnterprise:
		return fmt.Sprintf("ACCEPTED: contains Windows 11 Enterprise (x64). Import with --image-index %d.", rep.RecommendedIndex)
	case rep.AnyEval:
		return "REJECTED: this is an Evaluation ISO. import-disk-image needs a non-Eval Windows 11 Enterprise ISO from the M365 admin center."
	case rep.LooksConsumer:
		return "REJECTED: this looks like a consumer (Home/Pro) ISO. import-disk-image needs the Windows 11 Enterprise (business-editions) ISO from the M365 admin center."
	default:
		return "REJECTED: no Windows 11 Enterprise (x64) edition found. import-disk-image accepts Enterprise 23H2/24H2/25H2 x64 only."
	}
}

func utf16le(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		b[2*i] = byte(r)
		b[2*i+1] = byte(r >> 8)
	}
	return b
}

func decodeUTF16LE(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	// Strip a leading BOM if present.
	if len(u) > 0 && u[0] == 0xFEFF {
		u = u[1:]
	}
	return string(utf16.Decode(u))
}
