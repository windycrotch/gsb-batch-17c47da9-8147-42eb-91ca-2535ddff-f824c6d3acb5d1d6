package playlist

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/globocom/go-m3u8/internal"
	"github.com/rs/zerolog/log"
)

// Node names
const (
	commentNode         = "Comment"
	cueOutNode          = "CueOut"
	cueInNode           = "CueIn"
	dateRangeNode       = "DateRange"
	programDateTimeNode = "ProgramDateTime"
	extInfNode          = "ExtInf"
)

// Attribute keys
const (
	commentAttr   = "Comment"
	scte35OutAttr = "SCTE35-OUT"
	scte35InAttr  = "SCTE35-IN"
)

// Comments
const (
	spliceInsertComment   = "## splice_insert"
	programStartComment   = "## splice_insert(Program Start)"
	autoReturnModeComment = "## Auto Return Mode"
)

var (
	ErrParseLine = errors.New("failed to parse tag")
	ParamRegex   = regexp.MustCompile(`([a-zA-Z\d_-]+)=("[^"]+"|[^",]+)`)
)

type Playlist struct {
	*internal.DoublyLinkedList
	CurrentSegment        *ExtInfData
	CurrentStreamInf      *StreamInfData
	ProgramDateTime       time.Time
	MediaSequence         int
	DiscontinuitySequence int
	SegmentsCounter       int
	DVR                   float64
}

// Returns new Playlist instance with an empty doubly linked list
func NewPlaylist() *Playlist {
	return &Playlist{
		DoublyLinkedList:      new(internal.DoublyLinkedList),
		CurrentSegment:        nil,
		CurrentStreamInf:      nil,
		ProgramDateTime:       time.Time{},
		MediaSequence:         0,
		DiscontinuitySequence: 0,
		SegmentsCounter:       0,
		DVR:                   0,
	}
}

// Prints the playlist to stdout for debugging purposes
func (p *Playlist) Print() {
	if p.Head == nil || p.Tail == nil {
		log.Warn().Str("service", "go-m3u8/playlist.go").Msg("playlist is empty")
		return
	}

	current := p.Head
	for current != nil {
		fmt.Printf(">>>>>>>>> Node: %+v\n", current)
		if current.HLSElement != nil {
			fmt.Printf("HLSElement: %+v\n", current.HLSElement)
		}
		current = current.Next
	}
}

// Returns the Version (#EXT-X-VERSION) tag's value as a string
func (p *Playlist) VersionValue() string {
	node, found := p.Find("Version")
	if !found {
		return ""
	}
	return node.HLSElement.Attrs["#EXT-X-VERSION"]
}

// Returns the Version (#EXT-X-VERSION) tag as a Node if it exists, otherwise returns nil and false
func (p *Playlist) VersionTag() (*internal.Node, bool) {
	return p.Find("Version")
}

// Returns the MediaSequence (#EXT-X-MEDIA-SEQUENCE) tag's value as a string
func (p *Playlist) MediaSequenceValue() string {
	node, found := p.Find("MediaSequence")
	if !found {
		return ""
	}
	return node.HLSElement.Attrs["#EXT-X-MEDIA-SEQUENCE"]
}

// Returns the MediaSequence (#EXT-X-MEDIA-SEQUENCE) tag as a Node if it exists, otherwise returns nil and false
func (p *Playlist) MediaSequenceTag() (*internal.Node, bool) {
	return p.Find("MediaSequence")
}

// Returns the DiscontinuitySequence (#EXT-X-DISCONTINUITY-SEQUENCE) tag's value as a string
func (p *Playlist) DiscontinuitySequenceValue() string {
	node, found := p.Find("DiscontinuitySequence")
	if !found {
		return ""
	}
	return node.HLSElement.Attrs["#EXT-X-DISCONTINUITY-SEQUENCE"]
}

// Returns the DiscontinuitySequence (#EXT-X-DISCONTINUITY-SEQUENCE) tag as a Node if it exists, otherwise returns nil and false
func (p *Playlist) DiscontinuitySequenceTag() (*internal.Node, bool) {
	return p.Find("DiscontinuitySequence")
}

// Returns the VariableDefine (#EXT-X-DEFINE) tag as a Node if it exists, otherwise returns nil and false
func (p *Playlist) VariableDefineTag() (*internal.Node, bool) {
	return p.Find("VariableDefine")
}

// Returns all StreamInf (#EXT-X-STREAM-INF) nodes in the playlist
func (p *Playlist) Variants() []*internal.Node {
	return p.FindAll("StreamInf")
}

// Returns all Media (#EXT-X-MEDIA) nodes in the playlist (i.e. AUDIO groups, CLOSED-CAPTIONS groups, etc.)
func (p *Playlist) MediaGroups() []*internal.Node {
	return p.FindAll("Media")
}

// Returns all IFrameStreamInf (#EXT-X-I-FRAME-STREAM-INF) nodes in the playlist (i.e. keyframes)
func (p *Playlist) Keyframes() []*internal.Node {
	return p.FindAll("IFrameStreamInf")
}

// Returns all ExtInf (#EXTINF) nodes in the playlist
func (p *Playlist) Segments() []*internal.Node {
	return p.FindAll(extInfNode)
}

// Returns all Key (#EXT-X-KEY) nodes in the playlist
func (p *Playlist) EncryptionTags() []*internal.Node {
	return p.FindAll("Key")
}

// Returns all ProgramDateTime (#EXT-X-PROGRAM-DATE-TIME) nodes in the playlist
func (p *Playlist) ProgramDateTimeTags() []*internal.Node {
	return p.FindAll(programDateTimeNode)
}

// Returns all CueOut (#EXT-X-CUE-OUT) nodes in the playlist
func (p *Playlist) CueOutEvents() []*internal.Node {
	return p.FindAll(cueOutNode)
}

// Returns all CueIn (#EXT-X-CUE-IN) nodes in the playlist
func (p *Playlist) CueInEvents() []*internal.Node {
	return p.FindAll(cueInNode)
}

// Returns all DateRange (#EXT-X-DATERANGE) nodes with SCTE35-OUT marking in the playlist
func (p *Playlist) Breaks() []*internal.Node {
	result := make([]*internal.Node, 0)
	nodes := p.FindAll(dateRangeNode)
	for i := range nodes {
		if nodes[i].HLSElement.Attrs[scte35OutAttr] != "" {
			result = append(result, nodes[i])
		}
	}
	return result
}

// Returns all DateRange (#EXT-X-DATERANGE) nodes with SCTE35-IN marking in the playlist
func (p *Playlist) SCTE35InTags() []*internal.Node {
	result := make([]*internal.Node, 0)
	nodes := p.FindAll(dateRangeNode)
	for i := range nodes {
		if nodes[i].HLSElement.Attrs[scte35InAttr] != "" {
			result = append(result, nodes[i])
		}
	}
	return result
}

// Returns the first Comment node in the playlist whose value contains the given matchString.
//
//	Example: "# variants", "# AUDIO groups", etc
func (p *Playlist) Comment(matchString string) *internal.Node {
	nodes := p.FindAll(commentNode)
	for i := range nodes {
		if strings.Contains(nodes[i].HLSElement.Attrs[commentAttr], matchString) {
			return nodes[i]
		}
	}
	return nil
}

// Returns true if node is inside ad break and false otherwise.
// When true, method also returns the DateRange (#EXT-X-DATERANGE) object for the Ad Break.
//
// For entering the Ad Break, we always have DateRange tag with SCTE-OUT and CueOut (#EXT-X-CUE-OUT) tag.
// However, for exiting the Ad Break, we have three possible manifests:
//
//   - DateRange SCTE-IN is ALWAYS present.
//   - No DateRange SCTE-IN. Exit is ONLY marked by CueIn (#EXT-X-CUE-IN) tag instead.
//   - SOMETIMES DateRange SCTE-IN is present, alongside the CueIn tag.
func (p *Playlist) FindNodeInsideAdBreak(node *internal.Node) (*internal.Node, bool) {
	current := node.Prev
	for current != nil {
		// node is inside Ad Break if it is preceded by a DateRange tag with attribute SCTE35-OUT
		if (current.HLSElement.Name == dateRangeNode) && (current.HLSElement.Attrs[scte35OutAttr] != "") {
			return current, true
		}

		// node is outside Ad Break if it is preceded by a CueIn tag or a DateRange tag with attribute SCTE35-IN
		if (current.HLSElement.Name == cueInNode) || (current.HLSElement.Name == dateRangeNode && current.HLSElement.Attrs[scte35InAttr] != "") {
			return nil, false
		}

		current = current.Prev
	}

	return nil, false
}

// Returns the last DateRange (#EXT-X-DATERANGE) node with SCTE35-OUT marking (i.e. the last Ad Break) in the playlist
func (p *Playlist) FindLastAdBreak() (*internal.Node, bool) {
	adBreaks := p.Breaks()
	if len(adBreaks) == 0 {
		return nil, false
	}
	return adBreaks[len(adBreaks)-1], true
}

// FindBreakForEndMarker resolves which ad break owns an end marker (a CUE-IN
// tag or a DATERANGE SCTE35-IN tag). Unlike FindNodeInsideAdBreak, it does not
// stop at a DATERANGE SCTE35-IN: that tag is itself an end marker of the same
// break, so the search must pass through it in order to reach the owning
// DATERANGE SCTE35-OUT. It returns nil/false when the marker is not preceded by
// any ad break start.
func (p *Playlist) FindBreakForEndMarker(node *internal.Node) (*internal.Node, bool) {
	current := node.Prev
	for current != nil {
		if current.HLSElement.Name == dateRangeNode && current.HLSElement.Attrs[scte35OutAttr] != "" {
			return current, true
		}

		// A CUE-IN that belongs to an earlier (already terminated) break marks
		// the boundary of that break; an end marker past it cannot belong to a
		// break that started even earlier.
		if current.HLSElement.Name == cueInNode {
			return nil, false
		}

		current = current.Prev
	}

	return nil, false
}

// FindSecondLastAdBreak returns the DateRange node immediately preceding the last one, if it exists.
func (p *Playlist) FindSecondLastAdBreak() (*internal.Node, bool) {
	adBreaks := p.Breaks()
	if len(adBreaks) < 2 {
		return nil, false
	}
	return adBreaks[len(adBreaks)-2], true
}

// Returns the previous segment (#EXTINF) before the given node, or nil if none exists.
func (p *Playlist) FindPreviousSegment(node *internal.Node) *internal.Node {
	current := node.Prev
	for current != nil {
		if current.HLSElement.Name == extInfNode {
			return current
		}
		current = current.Prev
	}
	return nil
}

// Returns the next segment (#EXTINF) after the given node, or nil if none exists.
func (p *Playlist) FindNextSegment(node *internal.Node) *internal.Node {
	current := node.Next
	for current != nil {
		if current.HLSElement.Name == extInfNode {
			return current
		}
		current = current.Next
	}
	return nil
}

// Returns true if the given node is a segment (#EXTINF), false otherwise.
func IsSegment(node *internal.Node) bool {
	return node.HLSElement.Name == extInfNode
}

// Trims invalid and duplicated ad breaks from the playlist.
//
// The operation is idempotent: running it more than once on the same playlist
// produces exactly the same result as running it once. All nodes that are
// removed are collected first (based on break ownership) and removed in a
// single, guarded pass, so a node removed by an earlier step is never accessed
// again by a later step.
func (p *Playlist) TrimInvalidBreaks() {
	adBreaks := p.Breaks()
	if len(adBreaks) == 0 {
		// No ad breaks: nothing to do and nothing to break.
		return
	}

	// removed tracks every node scheduled for removal, making the operation
	// safe against stale references and repeated executions.
	removed := make(map[*internal.Node]bool)

	// surviving keeps the ad break nodes that remain in the playlist, in
	// playlist order. Duplicate detection always compares against the last
	// surviving break rather than against a possibly-removed snapshot entry.
	surviving := make([]*internal.Node, 0, len(adBreaks))

	for _, adBreak := range adBreaks {
		if removed[adBreak] {
			continue
		}

		if isInvalidBreak(adBreak) {
			for node := range p.breakNodesToRemove(adBreak) {
				removed[node] = true
			}
			continue
		}

		if len(surviving) > 0 {
			previous := surviving[len(surviving)-1]
			if IsDuplicatedProgramStartBreak(adBreak, previous) {
				// The Program Start break shares its CueIn with the remaining
				// break, so only its own start markers are removed.
				for node := range p.programStartBreakNodesToRemove(adBreak) {
					removed[node] = true
				}
				continue
			}
			if IsDuplicatedBreak(adBreak, previous) {
				// A duplicated break is a valid break, so its PROGRAM-DATE-TIME
				// tags stay (they also describe the surviving duplicate): only
				// the break's own start/end markers and comments are removed.
				for node := range p.duplicateBreakNodesToRemove(adBreak) {
					removed[node] = true
				}
				continue
			}
		}

		surviving = append(surviving, adBreak)
	}

	// Single guarded removal pass. Removing arbitrary nodes from a linked list
	// never invalidates the Head/Tail pointers of the remaining nodes.
	for node := range removed {
		p.Remove(node)
	}
}

// isInvalidBreak reports whether the ad break must be removed because its
// START-DATE cannot be parsed, its StartMediaSequence is not a valid integer,
// or its PLANNED-DURATION is missing, zero, non-numeric or longer than the
// 10 minutes limit.
func isInvalidBreak(adBreak *internal.Node) bool {
	if _, err := ValidateStartDate(adBreak); err != nil {
		return true
	}

	if _, err := ValidateMediaSequence(adBreak); err != nil {
		return true
	}

	return ValidatePlannedDuration(adBreak) != nil
}

// breakNodesToRemove collects every node that exists solely to serve the given
// invalid ad break: the start markers (DATERANGE SCTE35-OUT + CUE-OUT), the end
// markers (CUE-IN and DATERANGE SCTE35-IN) and the auxiliary nodes that belong
// only to this break (PROGRAM-DATE-TIME tags and related comments).
//
// Ownership is derived structurally (walking the linked list) instead of from
// fixed node positions, so the result is correct regardless of the exact node
// layout and never leaves orphaned CUE-OUT/CUE-IN markers behind.
func (p *Playlist) breakNodesToRemove(adBreak *internal.Node) map[*internal.Node]bool {
	nodes := p.breakMarkerNodesToRemove(adBreak)

	// Auxiliary start node: the PROGRAM-DATE-TIME that marks the break start.
	if pdt := p.breakStartProgramDateTime(adBreak); pdt != nil {
		nodes[pdt] = true
	}

	// Auxiliary end nodes: the PROGRAM-DATE-TIME immediately following each end
	// marker that belongs to this break.
	for _, cueIn := range p.CueInEvents() {
		owner, found := p.FindBreakForEndMarker(cueIn)
		if found && owner == adBreak {
			if pdt := nextNodeOfType(cueIn, programDateTimeNode); pdt != nil {
				nodes[pdt] = true
			}
		}
	}
	for _, scteIn := range p.SCTE35InTags() {
		owner, found := p.FindBreakForEndMarker(scteIn)
		if found && owner == adBreak {
			if pdt := nextNodeOfType(scteIn, programDateTimeNode); pdt != nil {
				nodes[pdt] = true
			}
		}
	}

	return nodes
}

// duplicateBreakNodesToRemove collects the markers of an ad break that is a
// duplicate of a surviving break. The break itself is valid, so the
// PROGRAM-DATE-TIME tags inside it are preserved (they equally describe the
// surviving duplicate); only the break's own start/end markers and comments are
// removed.
func (p *Playlist) duplicateBreakNodesToRemove(adBreak *internal.Node) map[*internal.Node]bool {
	return p.breakMarkerNodesToRemove(adBreak)
}

// breakMarkerNodesToRemove collects the start/end markers and the related
// comments of the given ad break, without touching PROGRAM-DATE-TIME tags.
func (p *Playlist) breakMarkerNodesToRemove(adBreak *internal.Node) map[*internal.Node]bool {
	nodes := map[*internal.Node]bool{adBreak: true}

	// Start markers: CUE-OUT immediately following the break, plus the
	// splice_insert comment that immediately precedes it.
	if cueOut := nextNodeOfType(adBreak, cueOutNode); cueOut != nil {
		nodes[cueOut] = true
		if comment := previousNodeOfType(cueOut, commentNode); comment != nil &&
			strings.Contains(comment.HLSElement.Attrs[commentAttr], spliceInsertComment) {
			nodes[comment] = true
		}
	}

	// End markers: every CUE-IN / DATERANGE SCTE35-IN that belongs to this
	// break, together with their auxiliary Auto Return Mode comments.
	for _, cueIn := range p.CueInEvents() {
		owner, found := p.FindBreakForEndMarker(cueIn)
		if !found || owner != adBreak {
			continue
		}
		nodes[cueIn] = true
		if comment := previousNodeOfType(cueIn, commentNode); comment != nil &&
			comment.HLSElement.Attrs[commentAttr] == autoReturnModeComment {
			nodes[comment] = true
		}
	}

	for _, scteIn := range p.SCTE35InTags() {
		owner, found := p.FindBreakForEndMarker(scteIn)
		if !found || owner != adBreak {
			continue
		}
		nodes[scteIn] = true
	}

	return nodes
}

// programStartBreakNodesToRemove collects the nodes of a duplicated Program
// Start break. Such a break shares its CUE-IN with the break that is kept, so
// only the Program Start comment, the DATERANGE and its own CUE-OUT are
// removed; the shared end markers must stay untouched.
func (p *Playlist) programStartBreakNodesToRemove(adBreak *internal.Node) map[*internal.Node]bool {
	nodes := map[*internal.Node]bool{adBreak: true}

	if cueOut := nextNodeOfType(adBreak, cueOutNode); cueOut != nil {
		nodes[cueOut] = true
	}

	if comment := previousNodeOfType(adBreak, commentNode); comment != nil &&
		strings.Contains(comment.HLSElement.Attrs[commentAttr], programStartComment) {
		nodes[comment] = true
	}

	return nodes
}

// breakStartProgramDateTime returns the PROGRAM-DATE-TIME node that marks the
// start of the given ad break, or nil when there is none. The tag normally
// follows the break's CUE-OUT; when the CUE-OUT has left the DVR window it
// directly follows the DATERANGE node.
func (p *Playlist) breakStartProgramDateTime(adBreak *internal.Node) *internal.Node {
	cursor := adBreak
	if cueOut := nextNodeOfType(adBreak, cueOutNode); cueOut != nil {
		cursor = cueOut
	}
	return nextNodeOfType(cursor, programDateTimeNode)
}

// nextNodeOfType returns the first node following node whose HLS element has
// the given name, stopping at the next media segment (#EXTINF), which marks a
// different logical block. It returns nil if no such node exists before then.
func nextNodeOfType(node *internal.Node, name string) *internal.Node {
	current := node.Next
	for current != nil {
		if current.HLSElement.Name == name {
			return current
		}
		if current.HLSElement.Name == extInfNode {
			return nil
		}
		current = current.Next
	}
	return nil
}

// previousNodeOfType returns the nearest node preceding node whose HLS element
// has the given name, stopping at the previous media segment (#EXTINF). It
// returns nil if no such node exists before then.
func previousNodeOfType(node *internal.Node, name string) *internal.Node {
	current := node.Prev
	for current != nil {
		if current.HLSElement.Name == name {
			return current
		}
		if current.HLSElement.Name == extInfNode {
			return nil
		}
		current = current.Prev
	}
	return nil
}
