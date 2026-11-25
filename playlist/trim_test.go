package playlist_test

import (
	"os"
	"testing"

	m3u8 "github.com/globocom/go-m3u8"
	pl "github.com/globocom/go-m3u8/playlist"
	"github.com/stretchr/testify/assert"
)

// parseHelper parses the mock playlist at the given path, failing the test on
// any error.
func parseHelper(t *testing.T, path string) *pl.Playlist {
	t.Helper()
	file, err := os.Open(path)
	assert.NoError(t, err)
	playlist, err := m3u8.ParsePlaylist(file)
	assert.NoError(t, err)
	return playlist
}

// trimAndEncode runs TrimInvalidBreaks and encodes the playlist back to text.
func trimAndEncode(t *testing.T, playlist *pl.Playlist) string {
	t.Helper()
	playlist.TrimInvalidBreaks()
	encoded, err := m3u8.EncodePlaylist(playlist)
	assert.NoError(t, err)
	return encoded
}

// assertIdempotent trims the playlist twice and asserts both encodings are
// byte-for-byte identical and that no node changes on the second trim.
func assertIdempotent(t *testing.T, path string) (string, *pl.Playlist) {
	t.Helper()
	playlist := parseHelper(t, path)
	first := trimAndEncode(t, playlist)
	second := trimAndEncode(t, playlist)
	assert.Equal(t, first, second, "trimming %s must be idempotent", path)
	return first, playlist
}

// TestTrimInvalidBreaksIsIdempotentAcrossMocks runs the parse -> trim ->
// encode -> trim -> encode pipeline over every playlist containing ad breaks
// and guarantees the two encodings match exactly.
func TestTrimInvalidBreaksIsIdempotentAcrossMocks(t *testing.T) {
	paths := []string{
		"./../mocks/media/media.m3u8",
		"./../mocks/media/withBreakInsideBreak.m3u8",
		"./../mocks/media/withDuplicatedBreaks.m3u8",
		"./../mocks/media/withInvalidBreaks.m3u8",
		"./../mocks/media/withMultipleBreaks.m3u8",
		"./../mocks/media/withProgramStartDuplicatedBreak.m3u8",
		"./../mocks/media/withDiscontinuity.m3u8",
		"./../mocks/media/withEncryptionAndSCTE35.m3u8",
		"./../mocks/media/withEndList.m3u8",
		"./../mocks/media/scte35/withCompleteAdBreak.m3u8",
		"./../mocks/media/scte35/withAdBreakNewReady.m3u8",
		"./../mocks/media/scte35/withAdBreakNewNotReady.m3u8",
		"./../mocks/media/scte35/withAdBreakNewReadyWithSegment.m3u8",
		"./../mocks/media/scte35/withAdBreakBeforeDVRLimit.m3u8",
		"./../mocks/media/scte35/withAdBreakOnDVRLimit.m3u8",
		"./../mocks/media/scte35/withAdBreakOutsideDVRLimit.m3u8",
		"./../mocks/media/scte35/withBreakStartTimePrecisionEx1.m3u8",
		"./../mocks/media/scte35/withBreakStartTimePrecisionEx2.m3u8",
		"./../mocks/media/scte35/withHLSInterstitials.m3u8",
		"./../mocks/media/withInvalidBreakAndSCTE35In.m3u8",
		"./../mocks/media/withInvalidBreakExceedsDuration.m3u8",
		"./../mocks/media/withInvalidBreakAtTail.m3u8",
		"./../mocks/media/withDuplicatedBreaksReversedOrder.m3u8",
		"./../mocks/media/withMultipleInvalidBreaks.m3u8",
		"./../mocks/media/withInvalidThenValidBreak.m3u8",
		"./../mocks/media/withInvalidBreakBetweenValidBreaks.m3u8",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			assertIdempotent(t, path)
		})
	}
}

// TestTrimInvalidBreaksNoBreaksDoesNotErrorOrChange verifies that a playlist
// without any ad break is returned unchanged (and without errors).
func TestTrimInvalidBreaksNoBreaksDoesNotErrorOrChange(t *testing.T) {
	path := "./../mocks/media/withNoBreaks.m3u8"
	pl := parseHelper(t, path)

	before, err := m3u8.EncodePlaylist(pl)
	assert.NoError(t, err)

	assert.NotPanics(t, func() { pl.TrimInvalidBreaks() })

	after, err := m3u8.EncodePlaylist(pl)
	assert.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Empty(t, pl.Breaks())
}

// TestTrimInvalidBreaksRemovesAllMarkersForInvalidBreak verifies the complete
// cleanup of an invalid break (PLANNED-DURATION == 0): the DATERANGE, CUE-OUT,
// CUE-IN, related comments and the break's PROGRAM-DATE-TIME tags all vanish,
// leaving no orphaned start or end markers.
func TestTrimInvalidBreaksRemovesAllMarkersForInvalidBreak(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withInvalidBreaks.m3u8")

	assert.Empty(t, pl.Breaks())
	assert.Empty(t, pl.CueOutEvents())
	assert.Empty(t, pl.CueInEvents())
	assert.Empty(t, pl.SCTE35InTags())
	// Only the playlist-level PDT (unrelated to the break) remains.
	assert.Len(t, pl.ProgramDateTimeTags(), 1)

	for _, marker := range []string{
		"#EXT-X-DATERANGE",
		"#EXT-X-CUE-OUT",
		"#EXT-X-CUE-IN",
		"## splice_insert",
		"## Auto Return Mode",
		"2025-05-20T20:24:52.699999Z", // break start PDT
		"2025-05-20T20:25:12.699999Z", // break end PDT
	} {
		assert.NotContains(t, encoded, marker, "removed break marker %q must not be re-encoded", marker)
	}
}

// TestTrimInvalidBreaksRemovesSCTE35InEndMarker covers a break whose end is
// marked both by a DATERANGE SCTE35-IN and a CUE-IN. When the break is invalid,
// both end markers (and their auxiliary nodes) must be removed together so the
// player never stays stuck in the ad state.
func TestTrimInvalidBreaksRemovesSCTE35InEndMarker(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withInvalidBreakAndSCTE35In.m3u8")

	assert.Empty(t, pl.Breaks())
	assert.Empty(t, pl.CueOutEvents())
	assert.Empty(t, pl.CueInEvents())
	assert.Empty(t, pl.SCTE35InTags())
	assert.Len(t, pl.ProgramDateTimeTags(), 1)

	for _, marker := range []string{
		"#EXT-X-DATERANGE",
		"#EXT-X-CUE-OUT",
		"#EXT-X-CUE-IN",
		"## Auto Return Mode",
	} {
		assert.NotContains(t, encoded, marker)
	}
}

// TestTrimInvalidBreaksDurationOverLimit verifies the 10 minutes threshold:
// a break with PLANNED-DURATION above 600s is treated as invalid and fully
// cleaned up.
func TestTrimInvalidBreaksDurationOverLimit(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withInvalidBreakExceedsDuration.m3u8")

	assert.Empty(t, pl.Breaks())
	assert.Empty(t, pl.CueOutEvents())
	assert.Empty(t, pl.CueInEvents())
	assert.NotContains(t, encoded, "#EXT-X-DATERANGE")
	assert.NotContains(t, encoded, "#EXT-X-CUE-OUT")
	assert.NotContains(t, encoded, "#EXT-X-CUE-IN")
}

// TestTrimInvalidBreaksMissingPlannedDurationAndAtTail makes sure an invalid
// break sitting at the playlist tail (no following nodes, no CUE-IN) is removed
// without panicking and leaves no orphaned CUE-OUT.
func TestTrimInvalidBreaksMissingPlannedDurationAndAtTail(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withInvalidBreakAtTail.m3u8")

	assert.Empty(t, pl.Breaks())
	assert.Empty(t, pl.CueOutEvents())
	assert.NotContains(t, encoded, "#EXT-X-DATERANGE")
	assert.NotContains(t, encoded, "#EXT-X-CUE-OUT")
}

// TestTrimInvalidBreaksMultipleInvalidBreaks verifies that removing one invalid
// break never corrupts the traversal for the next one: two consecutive invalid
// breaks are both fully removed in a single trim and the result is stable.
func TestTrimInvalidBreaksMultipleInvalidBreaks(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withMultipleInvalidBreaks.m3u8")

	assert.Empty(t, pl.Breaks())
	assert.Empty(t, pl.CueOutEvents())
	assert.Empty(t, pl.CueInEvents())
	assert.Len(t, pl.ProgramDateTimeTags(), 1)
	assert.NotContains(t, encoded, "#EXT-X-DATERANGE")
	assert.NotContains(t, encoded, "#EXT-X-CUE-OUT")
	assert.NotContains(t, encoded, "#EXT-X-CUE-IN")
}

// TestTrimDuplicatedBreaksKeepsFirst verifies that two breaks starting less
// than one second apart with the same PLANNED-DURATION are treated as
// duplicates and only the first one survives, while its retrieval methods keep
// working.
func TestTrimDuplicatedBreaksKeepsFirst(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withDuplicatedBreaks.m3u8")

	assert.Len(t, pl.Breaks(), 1)
	assert.Len(t, pl.CueOutEvents(), 1)
	assert.Len(t, pl.CueInEvents(), 1)
	// The first break (lower ID order in the mock) is the one kept.
	assert.Equal(t, "4026554821-1747772692", pl.Breaks()[0].HLSElement.Attrs["ID"])
	assert.NotContains(t, encoded, "4026554822-1747772692")
}

// TestTrimDuplicatedBreaksRegardlessOfOrder guarantees duplicate detection uses
// the absolute start-time difference: even when the later break in the file
// carries an earlier START-DATE, the first appearing break is retained.
func TestTrimDuplicatedBreaksRegardlessOfOrder(t *testing.T) {
	encoded, pl := assertIdempotent(t, "./../mocks/media/withDuplicatedBreaksReversedOrder.m3u8")

	assert.Len(t, pl.Breaks(), 1)
	assert.Equal(t, "4-1", pl.Breaks()[0].HLSElement.Attrs["ID"])
	assert.NotContains(t, encoded, "4-2")
}

// TestTrimProgramStartDuplicatedBreak verifies the shared-CUE-IN case: only the
// Program Start comment, DATERANGE and its CUE-OUT are removed; the surviving
// break keeps the shared CUE-IN and remains retrievable.
func TestTrimProgramStartDuplicatedBreak(t *testing.T) {
	expected, err := os.ReadFile("./../mocks/media/withProgramStartDuplicatedBreakTrimmed.m3u8")
	assert.NoError(t, err)

	pl := parseHelper(t, "./../mocks/media/withProgramStartDuplicatedBreak.m3u8")
	pl.TrimInvalidBreaks()
	encoded, err := m3u8.EncodePlaylist(pl)
	assert.NoError(t, err)
	assert.Equal(t, string(expected), encoded)

	// Second trim must not change anything.
	pl.TrimInvalidBreaks()
	second, err := m3u8.EncodePlaylist(pl)
	assert.NoError(t, err)
	assert.Equal(t, encoded, second)

	assert.Len(t, pl.Breaks(), 1)
	assert.Len(t, pl.CueOutEvents(), 1)
	assert.Len(t, pl.CueInEvents(), 1)
}

// TestTrimPreservesBreakOwnership verifies that after trimming, nodes inside a
// surviving ad break are still recognized as inside it while nodes outside are
// not.
func TestTrimPreservesBreakOwnership(t *testing.T) {
	_, pl := assertIdempotent(t, "./../mocks/media/withMultipleBreaks.m3u8")

	breaks := pl.Breaks()
	assert.Len(t, breaks, 2)

	segments := pl.Segments()
	// Collect the segments that fall inside each surviving break and make sure
	// ownership resolves to the correct break; every other segment is outside.
	insideCount := 0
	for _, segment := range segments {
		owner, inside := pl.FindNodeInsideAdBreak(segment)
		if inside {
			insideCount++
			assert.Contains(t, breaks, owner)
		}
	}
	assert.Greater(t, insideCount, 0, "at least one segment must remain inside a break")

	// The first segment precedes every break and must be outside.
	owner, inside := pl.FindNodeInsideAdBreak(segments[0])
	assert.False(t, inside)
	assert.Nil(t, owner)
}

// TestTrimSurvivingBreakMarkersRetrievable ensures the retained break's start
// and end markers remain discoverable through the public retrieval APIs after a
// trim cycle.
func TestTrimSurvivingBreakMarkersRetrievable(t *testing.T) {
	_, pl := assertIdempotent(t, "./../mocks/media/media.m3u8")

	assert.Len(t, pl.Breaks(), 1)
	assert.Len(t, pl.CueOutEvents(), 1)
	assert.Len(t, pl.CueInEvents(), 1)
	assert.Len(t, pl.SCTE35InTags(), 1)

	// A segment inside the surviving break still resolves to that break.
	insideSegment := pl.Segments()[6]
	owner, inside := pl.FindNodeInsideAdBreak(insideSegment)
	assert.True(t, inside)
	assert.Equal(t, pl.Breaks()[0], owner)

	// A segment after the break remains outside.
	afterSegment := pl.Segments()[18]
	owner, inside = pl.FindNodeInsideAdBreak(afterSegment)
	assert.False(t, inside)
	assert.Nil(t, owner)
}

// TestTrimDoesNotTouchSegments guarantees trimming only removes signaling
// DATERANGE/CUE/PDT/comment nodes and never removes media segments.
func TestTrimDoesNotTouchSegments(t *testing.T) {
	original := parseHelper(t, "./../mocks/media/withInvalidBreaks.m3u8")
	expectedSegments := len(original.Segments())

	_, trimmed := assertIdempotent(t, "./../mocks/media/withInvalidBreaks.m3u8")
	assert.Len(t, trimmed.Segments(), expectedSegments)
}

// TestTrimInvalidBreakFollowedByValidBreak makes sure removing an invalid
// break does not damage the valid break that follows it: the valid break's
// markers stay present and its inside/outside ownership stays correct.
func TestTrimInvalidBreakFollowedByValidBreak(t *testing.T) {
	encoded, playlist := assertIdempotent(t, "./../mocks/media/withInvalidThenValidBreak.m3u8")

	assert.Len(t, playlist.Breaks(), 1)
	assert.Len(t, playlist.CueOutEvents(), 1)
	assert.Len(t, playlist.CueInEvents(), 1)
	assert.Equal(t, "6-2", playlist.Breaks()[0].HLSElement.Attrs["ID"])

	// The invalid break markers are gone.
	assert.NotContains(t, encoded, "6-1")
	// The valid break markers survive.
	assert.Contains(t, encoded, "6-2")
	assert.Contains(t, encoded, "#EXT-X-CUE-OUT")
	assert.Contains(t, encoded, "#EXT-X-CUE-IN")

	// Segments belonging to the surviving break are still recognized.
	owner, inside := playlist.FindNodeInsideAdBreak(playlist.Segments()[3])
	assert.True(t, inside)
	assert.Equal(t, playlist.Breaks()[0], owner)
}

// TestTrimInvalidBreakBetweenValidBreaks verifies that when the middle of three
// breaks is invalid, only that one is removed and both surrounding valid breaks
// keep their markers and ownership.
func TestTrimInvalidBreakBetweenValidBreaks(t *testing.T) {
	encoded, playlist := assertIdempotent(t, "./../mocks/media/withInvalidBreakBetweenValidBreaks.m3u8")

	assert.Len(t, playlist.Breaks(), 2)
	assert.Len(t, playlist.CueOutEvents(), 2)
	assert.Len(t, playlist.CueInEvents(), 2)
	assert.NotContains(t, encoded, "7-2")
	assert.Contains(t, encoded, "7-1")
	assert.Contains(t, encoded, "7-3")
}

// TestFindBreakForEndMarkerSkipsSCTE35In is a focused structural check that the
// end-marker ownership helper resolves through a DATERANGE SCTE35-IN to the
// owning SCTE35-OUT break.
func TestFindBreakForEndMarkerSkipsSCTE35In(t *testing.T) {
	pl := parseHelper(t, "./../mocks/media/media.m3u8")

	breakNode, ok := pl.FindLastAdBreak()
	assert.True(t, ok)

	cueIn := pl.CueInEvents()[0]
	owner, found := pl.FindBreakForEndMarker(cueIn)
	assert.True(t, found)
	assert.Equal(t, breakNode, owner)

	scteIn := pl.SCTE35InTags()[0]
	owner, found = pl.FindBreakForEndMarker(scteIn)
	assert.True(t, found)
	assert.Equal(t, breakNode, owner)
}
