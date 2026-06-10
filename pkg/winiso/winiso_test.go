package winiso

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf16"
)

// encodeISO wraps a WIM XML document as UTF-16LE and embeds it in some filler
// bytes, mimicking how install.wim metadata sits inside an ISO byte stream.
func encodeISO(xmlDoc string) []byte {
	u := utf16.Encode([]rune(xmlDoc))
	b := make([]byte, len(u)*2)
	for i, r := range u {
		b[2*i] = byte(r)
		b[2*i+1] = byte(r >> 8)
	}
	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte{0x00, 0xAB}, 4096)) // leading filler
	buf.Write(b)
	buf.Write(bytes.Repeat([]byte{0xCD, 0x00}, 4096)) // trailing filler
	return buf.Bytes()
}

func image(idx int, name, editionID string, arch int) string {
	return `<IMAGE INDEX="` + itoa(idx) + `"><WINDOWS><ARCH>` + itoa(arch) +
		`</ARCH><EDITIONID>` + editionID + `</EDITIONID><INSTALLATIONTYPE>Client</INSTALLATIONTYPE>` +
		`<VERSION><BUILD>26200</BUILD></VERSION></WINDOWS>` +
		`<FLAGS>` + editionID + `</FLAGS><NAME>` + name + `</NAME></IMAGE>`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var s string
	for i > 0 {
		s = string(rune('0'+i%10)) + s
		i /= 10
	}
	return s
}

func TestInspect_BusinessISO(t *testing.T) {
	doc := "<WIM>" +
		image(1, "Windows 11 Education", "Education", 9) +
		image(2, "Windows 11 Education N", "EducationN", 9) +
		image(3, "Windows 11 Enterprise", "Enterprise", 9) +
		image(4, "Windows 11 Enterprise N", "EnterpriseN", 9) +
		image(5, "Windows 11 Pro", "Professional", 9) +
		"</WIM>"
	rep, err := Inspect(bytes.NewReader(encodeISO(doc)))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(rep.Editions) != 5 {
		t.Fatalf("expected 5 editions, got %d", len(rep.Editions))
	}
	if !rep.Acceptable || !rep.HasEnterprise {
		t.Error("business ISO with Enterprise must be acceptable")
	}
	if rep.RecommendedIndex != 3 {
		t.Errorf("recommended index = %d, want 3 (plain Enterprise)", rep.RecommendedIndex)
	}
	if rep.AnyEval {
		t.Error("non-eval ISO must not be flagged AnyEval")
	}
	// Enterprise indexes supported; Pro/Education not.
	for _, e := range rep.Editions {
		wantSup := e.Index == 3 || e.Index == 4
		if e.Supported != wantSup {
			t.Errorf("index %d (%s) Supported=%v, want %v", e.Index, e.EditionID, e.Supported, wantSup)
		}
	}
}

func TestInspect_Dedupe(t *testing.T) {
	// Same WIM block twice (tail rescan scenario) must not double the editions.
	one := "<WIM>" + image(3, "Windows 11 Enterprise", "Enterprise", 9) + "</WIM>"
	iso := append(encodeISO(one), encodeISO(one)...)
	rep, err := Inspect(bytes.NewReader(iso))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(rep.Editions) != 1 {
		t.Fatalf("dedupe failed: got %d editions, want 1", len(rep.Editions))
	}
}

func TestInspect_EvalRejected(t *testing.T) {
	doc := "<WIM>" + image(1, "Windows 11 Enterprise Evaluation", "EnterpriseEval", 9) + "</WIM>"
	rep, err := Inspect(bytes.NewReader(encodeISO(doc)))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if rep.Acceptable {
		t.Error("Evaluation ISO must not be acceptable")
	}
	if !rep.AnyEval {
		t.Error("Evaluation must be detected")
	}
	if !strings.Contains(rep.Summary, "Evaluation") {
		t.Errorf("summary should mention Evaluation, got %q", rep.Summary)
	}
}

func TestInspect_ConsumerRejected(t *testing.T) {
	doc := "<WIM>" +
		image(1, "Windows 11 Home", "Core", 9) +
		image(2, "Windows 11 Pro", "Professional", 9) +
		"</WIM>"
	rep, err := Inspect(bytes.NewReader(encodeISO(doc)))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if rep.Acceptable {
		t.Error("consumer ISO must not be acceptable")
	}
	if !rep.LooksConsumer {
		t.Error("Home present + no Enterprise should set LooksConsumer")
	}
	if !strings.Contains(rep.Summary, "consumer") {
		t.Errorf("summary should mention consumer, got %q", rep.Summary)
	}
}

func TestInspect_ARM64EnterpriseNotSupported(t *testing.T) {
	// Enterprise but arm64 — import-disk-image is x64-only.
	doc := "<WIM>" + image(1, "Windows 11 Enterprise", "Enterprise", 12) + "</WIM>"
	rep, err := Inspect(bytes.NewReader(encodeISO(doc)))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if rep.Acceptable {
		t.Error("arm64 Enterprise must not be acceptable (x64-only)")
	}
	if rep.Editions[0].Arch != "arm64" {
		t.Errorf("arch = %q, want arm64", rep.Editions[0].Arch)
	}
}

func TestInspect_IgnoresWindowsPE(t *testing.T) {
	// boot.wim's WindowsPE images must be ignored.
	pe := `<IMAGE INDEX="1"><WINDOWS><ARCH>9</ARCH><INSTALLATIONTYPE>WindowsPE</INSTALLATIONTYPE></WINDOWS><NAME>Windows PE</NAME></IMAGE>`
	doc := "<WIM>" + pe + image(3, "Windows 11 Enterprise", "Enterprise", 9) + "</WIM>"
	rep, err := Inspect(bytes.NewReader(encodeISO(doc)))
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(rep.Editions) != 1 || rep.Editions[0].Index != 3 {
		t.Fatalf("WindowsPE image must be ignored; got %+v", rep.Editions)
	}
}

func TestInspect_NoWIM(t *testing.T) {
	if _, err := Inspect(bytes.NewReader([]byte("not an iso, no wim metadata here"))); err == nil {
		t.Error("expected error when no WIM metadata present")
	}
}
