// What an image records about the spawn and the instance that made it (#1364).

package aws

import (
	"fmt"
	"strings"
)

// ImageSpawnVersionTagKey records, on an image spawn creates, the version of
// the spawn CLI that created it. An image without it was made by a spawn from
// before #1364, or outside spawn.
const ImageSpawnVersionTagKey = "spawn:image-spawn-version"

// Tags copied from the source instance onto an image created from it, under
// a spawn:source-* key (#1364). A source's tags disappear about an hour after
// it terminates, and an MPI cohort member rarely lives that long, so the image
// is the only place this survives. Each is something a fix can make an image
// unsafe or stale by, so that a later "images made before fix X" check can
// recognise the image:
//
//   - spawn:version, the spawn that launched the source. It chose the
//     bootstrap that configured the disk being imaged (#1338/#1343: a
//     non-zero MPI rank launched before v0.147.0 has rank 0's key in root's
//     authorized_keys without the spawn-mpi-cohort marker).
//   - spawn:spored-version, the spored running on the source, which is the
//     spored baked into the image. It can differ from spawn:version after
//     `spawn upgrade-spored`, and a spored fix (its first-boot cleanup of
//     inherited state, for one) is keyed on this, not on the launching CLI.
//   - spawn:mpi-enabled, whether the source was an MPI cohort member, which
//     is what puts a cluster key and an MPI hostfile on the disk.
//   - spawn:job-array-index, its rank in the cohort. Rank 0 and the other
//     ranks carry different keys (#1343), so a check needs the rank, not just
//     cohort membership.
//
// Not copied: spawn:job-array-id/-name/-created, which name a cohort that is
// gone by the time anyone asks, and spawn:job-array-size, which says nothing
// about this rank's disk that the index does not.
const (
	SourceVersionTagKey       = "spawn:source-version"
	SourceSporedVersionTagKey = "spawn:source-spored-version"
	SourceMPIEnabledTagKey    = "spawn:source-mpi-enabled"
	SourceJobArrayIndexTagKey = "spawn:source-job-array-index"
)

// ImageProvenanceTagKeys are the tags only spawn writes on an image it
// creates. A user's --tag cannot set them: a forged spawn:source-version would
// tell a future check that an image predating a fix was made after it.
func ImageProvenanceTagKeys() []string {
	return []string{
		ImageSpawnVersionTagKey,
		SourceVersionTagKey,
		SourceSporedVersionTagKey,
		SourceMPIEnabledTagKey,
		SourceJobArrayIndexTagKey,
	}
}

// IsImageProvenanceTagKey reports whether key is one of ImageProvenanceTagKeys.
func IsImageProvenanceTagKey(key string) bool {
	for _, k := range ImageProvenanceTagKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// ImageCreateTags is the tag set for an image (and its snapshots) created
// from an instance carrying sourceTags: userTags without any provenance key,
// then spawn:image-spawn-version (CallerVersion, omitted when unset rather
// than written as a guess) and a spawn:source-* copy of each source tag the
// instance has. A source tag the instance lacks is left out, not written
// empty, so "absent" keeps meaning "not known".
func ImageCreateTags(userTags, sourceTags map[string]string) map[string]string {
	out := make(map[string]string, len(userTags)+5)
	for k, v := range userTags {
		if IsImageProvenanceTagKey(k) {
			continue
		}
		out[k] = v
	}
	if CallerVersion != "" {
		out[ImageSpawnVersionTagKey] = CallerVersion
	}
	// One assignment per key, spelled out, so the tag-contract scan
	// (tag_contract_test.go) sees each write.
	if v := sourceTags["spawn:version"]; v != "" {
		out[SourceVersionTagKey] = v
	}
	if v := sourceTags["spawn:spored-version"]; v != "" {
		out[SourceSporedVersionTagKey] = v
	}
	if v := sourceTags["spawn:mpi-enabled"]; v != "" {
		out[SourceMPIEnabledTagKey] = v
	}
	if v := sourceTags["spawn:job-array-index"]; v != "" {
		out[SourceJobArrayIndexTagKey] = v
	}
	return out
}

// checkImageTagCount refuses an image tag set that EC2 would reject: images
// and snapshots have the same 50-tag limit as instances, and CreateImage and
// CopyImage fail outright rather than truncate. The spawn: tags are what
// `spawn ami list`, `spawn footprint` and `spawn ami delete` find the image by,
// so it is the user's tags that must go.
func checkImageTagCount(action string, tags map[string]string) error {
	if len(tags) <= awsMaxTagsPerResource {
		return nil
	}
	custom := 0
	for k := range tags {
		if !strings.HasPrefix(k, "spawn:") {
			custom++
		}
	}
	return fmt.Errorf("refusing to %s: the image would carry %d tags and AWS allows %d per resource, "+
		"so EC2 would reject it. Drop at least %d of the %d non-spawn tag(s); the spawn: tags are what spawn "+
		"lists, counts and deletes the image by", action, len(tags), awsMaxTagsPerResource,
		len(tags)-awsMaxTagsPerResource, custom)
}

// ImageProvenance is what an image records about the spawn that created it
// and the instance it was created from (#1364). A field is empty when the
// image does not carry the tag: made by a spawn from before #1364, from a
// source without it, or outside spawn.
type ImageProvenance struct {
	// SpawnVersion is the spawn CLI that created the image
	// (spawn:image-spawn-version).
	SpawnVersion string `json:"image_spawn_version,omitempty"`
	// SourceVersion is the spawn that launched the source instance.
	SourceVersion string `json:"source_version,omitempty"`
	// SourceSporedVersion is the spored the source ran, baked into the image.
	SourceSporedVersion string `json:"source_spored_version,omitempty"`
	// SourceMPIEnabled is the source's spawn:mpi-enabled ("true" for an MPI
	// cohort member).
	SourceMPIEnabled string `json:"source_mpi_enabled,omitempty"`
	// SourceJobArrayIndex is the source's rank in its job array or MPI
	// cohort, as tagged ("0" is rank 0).
	SourceJobArrayIndex string `json:"source_job_array_index,omitempty"`
}

// ImageProvenanceFromTags reads an image's provenance tags. It makes no
// judgement about them; a check for images made before a fix keys on the
// fields (an empty SpawnVersion meaning the image predates #1364).
func ImageProvenanceFromTags(tags map[string]string) ImageProvenance {
	return ImageProvenance{
		SpawnVersion:        tags[ImageSpawnVersionTagKey],
		SourceVersion:       tags[SourceVersionTagKey],
		SourceSporedVersion: tags[SourceSporedVersionTagKey],
		SourceMPIEnabled:    tags[SourceMPIEnabledTagKey],
		SourceJobArrayIndex: tags[SourceJobArrayIndexTagKey],
	}
}

// Recorded reports whether the image carries any provenance tag.
func (p ImageProvenance) Recorded() bool {
	return p != ImageProvenance{}
}
