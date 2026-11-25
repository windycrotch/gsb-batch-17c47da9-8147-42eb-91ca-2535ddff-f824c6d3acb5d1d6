package merge

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// readersSee checks what a reader of the output trusts behind its head:
// every slot it offers is the INVALID grain, or a member that holds the
// index the slot stands for.
func readersSee(t *testing.T, d *Discrete, ms []Member, ctx string) {
	t.Helper()
	for i := max(d.Head+2-uint64(d.Grains), 1); i <= d.Head; i++ {
		slot := uint(i % uint64(d.Grains))
		src := d.Current[slot]
		if src == d.Placeholder {
			continue
		}
		require.GreaterOrEqual(t, src, 0, "%s: index %d points at source %d", ctx, i, src)
		require.Less(t, src, len(ms), "%s: index %d points at source %d", ctx, i, src)
		g := ms[src].Grain(slot)
		require.Equal(t, i, g.Index, "%s: head %d offers index %d in slot %d from member %d, which holds %d",
			ctx, d.Head, i, slot, src, g.Index)
		require.False(t, g.Invalid, "%s: index %d offered from member %d as INVALID", ctx, i, src)
	}
}

// A restarted producer's ring starts at a later index than the output had
// published: the indices below the new ring's head were never its to
// write. It is late with them, as a late primary is, rather than taken to
// have skipped them: the output waits for it, then serves the others
// until its ring reaches an index of its own, and it stays the primary.
func TestDiscreteWaitsForANewRingThatStartsLater(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew, d.Tolerance = true, 8, 5*time.Millisecond
	now := time.Unix(0, 0)
	for i := uint64(100); i <= 119; i++ {
		a.commit(i)
		b.commit(i)
		_, _, _ = d.Step([]Member{a.member(), b.member()}, now)
	}
	require.Equal(t, uint64(119), d.Head)
	d.Drop([]Member{a.member(), b.member()}, 0)

	// The new ring starts at 124; b goes on from 120.
	fresh := newRing(grains)
	for i := uint64(120); i <= 130; i++ {
		b.commit(i)
		if i >= 124 {
			fresh.commit(i)
		}
	}
	ms := []Member{fresh.member(), b.member()}
	_, head, wait := d.Step(ms, now.Add(time.Millisecond))
	require.Equal(t, uint64(119), head, "the output published past the new ring before it arrived")
	require.Positive(t, wait, "the new ring was not waited for")
	require.LessOrEqual(t, wait, d.Tolerance)

	sel, head, _ := d.Step(ms, now.Add(time.Millisecond).Add(wait))
	require.Equal(t, uint64(130), head)
	require.Equal(t, 0, d.Primary, "the new ring lost the primary for having started later")
	for _, s := range sel {
		if s.Index < 124 {
			require.Equal(t, 1, s.Source, "index %d, which the new ring never wrote, served from it", s.Index)
		} else {
			require.Equal(t, 0, s.Source, "index %d, which the new ring wrote, not served from it", s.Index)
		}
	}
	readersSee(t, d, ms, "after the new ring arrived")
}

// Drop moves every slot the replaced member served before its new ring is
// wired in, and the output keeps publishing from the others while the new
// ring catches up.
func TestDiscreteReplacementKeepsPublishing(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew, d.Tolerance = true, 8, 5*time.Millisecond
	now := time.Unix(0, 0)
	for i := uint64(100); i <= 119; i++ {
		a.commit(i)
		b.commit(i)
		_, _, _ = d.Step([]Member{a.member(), b.member()}, now)
	}
	sel := d.Drop([]Member{a.member(), b.member()}, 0)
	require.NotEmpty(t, sel)
	for _, s := range sel {
		require.NotEqual(t, 0, s.Source, "slot %d left on the replaced member", s.Slot)
	}
	for slot, src := range d.Current {
		require.NotEqual(t, 0, src, "slot %d left on the replaced member", slot)
	}

	// b carries on; the new ring joins at 124.
	fresh := newRing(grains)
	for i := uint64(120); i <= 150; i++ {
		b.commit(i)
		if i >= 124 {
			fresh.commit(i)
		}
		ms := []Member{fresh.member(), b.member()}
		now = now.Add(time.Millisecond)
		_, head, wait := d.Step(ms, now)
		if wait > 0 {
			_, head, _ = d.Step(ms, now.Add(wait))
		}
		require.Equal(t, i, head, "index %d was not published", i)
		readersSee(t, d, ms, "after the replacement")
	}
}

// The old ring's committed but unpublished range is not the new ring's:
// after the replacement the output does not publish it from the member.
func TestDiscreteOldRingsRangeIsNotTheNewRings(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew, d.Tolerance = true, 8, 5*time.Millisecond
	now := time.Unix(0, 0)
	for i := uint64(100); i <= 119; i++ {
		a.commit(i)
		b.commit(i)
		_, _, _ = d.Step([]Member{a.member(), b.member()}, now)
	}
	for i := uint64(120); i <= 123; i++ {
		a.commit(i) // committed, no pass published it
	}
	d.Drop([]Member{a.member(), b.member()}, 0)

	fresh := newRing(grains) // holds none of 120..123
	for i := uint64(120); i <= 135; i++ {
		b.commit(i)
		ms := []Member{fresh.member(), b.member()}
		now = now.Add(time.Millisecond)
		_, head, wait := d.Step(ms, now)
		if wait > 0 {
			_, _, _ = d.Step(ms, now.Add(wait))
		}
		for j := uint64(120); j <= head; j++ {
			require.NotEqual(t, 0, d.Current[j%grains], "index %d published from the replaced member", j)
		}
		readersSee(t, d, ms, "old range")
	}
}

// Retire runs in the pass that replaces a member, over the members as the
// pass before the swap described them. It must not move a slot back onto
// the replaced member, whose ring holds none of them, nor let the index a
// reader sees go back.
func TestDiscreteRetireDoesNotMoveSlotsOntoAReplacedMember(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew = true, 8
	now := time.Unix(0, 0)
	for i := uint64(100); i <= 119; i++ {
		a.commit(i)
		b.commit(i)
		ms := []Member{a.member(), b.member()}
		d.Step(ms, now)
		d.Retire(ms, i)
	}
	for i := uint64(120); i <= 135; i++ {
		b.commit(i)
	}
	ms := []Member{a.member(), b.member()}
	before := d.Head
	d.Drop(ms, 0)
	sel := d.Retire(ms, 135)
	for _, s := range sel {
		require.NotEqual(t, 0, s.Source, "slot %d (index %d) moved back onto the replaced member", s.Slot, s.Index)
	}
	for slot, src := range d.Current {
		require.NotEqual(t, 0, src, "slot %d left on the replaced member", slot)
	}
	require.Equal(t, before, d.Head, "the published index went back")
}

// A member replaced while the output lagged it: the slots Retire moves go
// to the members that still hold them, never to the new ring.
func TestDiscreteRetirePrefersAHolderOverAReplacement(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew = true, 10
	for i := uint64(90); i <= 128; i++ {
		if i != 120 {
			a.commit(i)
		}
	}
	for i := uint64(90); i <= 123; i++ {
		if i != 120 {
			b.commit(i)
		}
	}
	d.Head, d.Primary = 119, 0
	for i := range d.Current {
		d.Current[i] = 0
	}
	ms := []Member{a.member(), b.member()}
	d.Drop(ms, 0)
	for _, s := range d.Retire(ms, 123) {
		require.NotEqual(t, 0, s.Source, "slot %d moved onto the replaced member", s.Slot)
		require.Equal(t, 1, s.Source, "slot %d moved to a member that does not hold it", s.Slot)
	}
}

// Audio: the wait for a late primary belongs to the ring that was late.
// A ring replaced while a wait was under way is waited for afresh from
// the replacement, rather than passed over on the old ring's clock.
func TestAudioReplacementGetsItsOwnWait(t *testing.T) {
	a := NewAudio(2, 38, 2)
	a.Skew, a.Tolerance = 2048, 20*time.Millisecond
	for n := uint64(10); n <= 20; n++ {
		a.Committed(0, n*PageSamples, (n+1)*PageSamples)
		a.Committed(1, n*PageSamples, (n+1)*PageSamples)
	}
	now := time.Unix(0, 0)
	_, head, _ := a.Step(now)
	require.Equal(t, uint64(21*PageSamples), head)

	// The primary falls behind: a wait begins and runs out.
	a.Committed(1, 21*PageSamples, 22*PageSamples)
	_, head, wait := a.Step(now)
	require.Equal(t, uint64(21*PageSamples), head)
	require.Equal(t, a.Tolerance, wait)

	// Its producer restarts before the wait resolves; the new ring is put
	// in place in the next pass, whose clock the wait dates from.
	a.Drop(0)
	a.Committed(0, 25*PageSamples, 26*PageSamples) // starts later
	sel, head, wait := a.Step(now.Add(time.Millisecond))
	require.Equal(t, uint64(21*PageSamples), head, "the new ring was passed over on the old ring's clock")
	require.Positive(t, wait, "the new ring got no wait of its own")
	for _, s := range sel {
		require.NotEqual(t, 0, s.Source, "page %d served from the replaced member", s.Page)
	}
}

// Audio: a replaced member's pages move to the one that still holds them,
// and its new ring, which starts later, is waited for rather than served
// from the range the old ring wrote.
func TestAudioWaitsForANewRingThatStartsLater(t *testing.T) {
	a := NewAudio(2, 38, 2)
	a.Skew, a.Tolerance = 2048, 5*time.Millisecond
	for n := uint64(10); n <= 20; n++ {
		a.Committed(0, n*PageSamples, (n+1)*PageSamples)
		a.Committed(1, n*PageSamples, (n+1)*PageSamples)
	}
	now := time.Unix(0, 0)
	_, head, _ := a.Step(now)
	require.Equal(t, uint64(21*PageSamples), head)
	a.Drop(0)

	// The new ring starts at page 25; b holds 21..29.
	a.Committed(0, 25*PageSamples, 30*PageSamples)
	a.Committed(1, 21*PageSamples, 30*PageSamples)
	sel, head, wait := a.Step(now.Add(time.Millisecond))
	require.Empty(t, sel)
	require.Equal(t, uint64(21*PageSamples), head, "the output published pages the new ring never held")
	require.Positive(t, wait, "the new ring was not waited for")

	sel, head, _ = a.Step(now.Add(time.Millisecond).Add(wait))
	require.Equal(t, uint64(30*PageSamples), head)
	require.Equal(t, 0, a.Primary, "the new ring lost the primary for having started later")
	for _, s := range sel {
		if uint64(s.Page) < 25 {
			require.Equal(t, 1, s.Source, "page %d, which the new ring never held, served from it", s.Page)
		}
	}
}

// Audio: a replaced primary's pages never come back to it, and its new
// ring is not taken to hold what the old one did.
func TestAudioDropMovesPagesAndForgetsTheOldRing(t *testing.T) {
	a := NewAudio(2, 38, 2)
	a.Skew = 2048
	for n := uint64(10); n <= 40; n++ {
		a.Committed(0, n*PageSamples, (n+1)*PageSamples)
		a.Committed(1, n*PageSamples, (n+1)*PageSamples)
	}
	_, head, _ := a.Step(time.Unix(0, 0))
	require.Equal(t, uint64(41*PageSamples), head)
	a.Committed(0, 41*PageSamples, 44*PageSamples) // the old ring's last, unpublished

	sel := a.Drop(0)
	require.NotEmpty(t, sel)
	for _, s := range sel {
		require.NotEqual(t, 0, s.Source, "page slot %d left on the replaced member", s.Page)
	}
	for p, src := range a.Current {
		require.NotEqual(t, 0, src, "page slot %d left on the replaced member", p)
	}

	// The new ring starts at page 50. Nothing of 41..49 is taken from it.
	a.Committed(1, 41*PageSamples, 51*PageSamples)
	a.Committed(0, 50*PageSamples, 51*PageSamples)
	_, head, wait := a.Step(time.Unix(0, 0).Add(time.Millisecond))
	if wait > 0 {
		_, head, _ = a.Step(time.Unix(0, 0).Add(time.Millisecond).Add(wait))
	}
	require.Equal(t, uint64(51*PageSamples), head)
	for n := uint64(41); n < 50; n++ {
		require.Equal(t, 1, a.Current[n%a.Pages], "page %d, which the new ring never held, is served from it", n)
	}
	require.Equal(t, 0, a.Current[50%a.Pages], "page 50, which the new ring holds, is not served from it")
}

// A wait for a late primary is one wait per lateness, not one per pass:
// the engine passes many times while it runs, and the deadline must
// count down rather than restart at every pass.
func TestDiscreteWaitsForALatePrimaryOnce(t *testing.T) {
	const grains = 20
	a, b := newRing(grains), newRing(grains)
	d := NewDiscrete(grains, 2)
	d.Ordered, d.Skew = true, 8
	d.Tolerance = 20 * time.Millisecond
	now := time.Unix(0, 0)
	for i := uint64(10); i <= 20; i++ {
		a.commit(i)
		b.commit(i)
	}
	d.Step([]Member{a.member(), b.member()}, now)
	b.commit(21)

	prev := d.Tolerance + 1
	for k := 0; k < 6; k++ {
		_, head, wait := d.Step([]Member{a.member(), b.member()}, now.Add(time.Duration(k)*time.Millisecond))
		require.Equal(t, uint64(20), head, "pass %d published past the late primary", k)
		require.LessOrEqual(t, wait, prev, "pass %d restarted the wait", k)
		prev = wait
	}
}

// The audio counterpart: the wait counts down across passes rather than
// starting over, so the output is held back one Tolerance, not one per
// pass while the primary stays late.
func TestAudioWaitsForALatePrimaryOnce(t *testing.T) {
	a := NewAudio(2, 38, 2)
	a.Skew, a.Tolerance = 2048, 20*time.Millisecond
	for m := 0; m < 2; m++ {
		a.Committed(m, 10*PageSamples, 20*PageSamples)
	}
	now := time.Unix(0, 0)
	a.Step(now)
	a.Committed(1, 20*PageSamples, 21*PageSamples)

	prev := a.Tolerance + 1
	for k := 0; k < 6; k++ {
		_, head, wait := a.Step(now.Add(time.Duration(k) * time.Millisecond))
		require.Equal(t, uint64(20*PageSamples), head, "pass %d published past the late primary", k)
		require.LessOrEqual(t, wait, prev, "pass %d restarted the wait", k)
		prev = wait
	}
}

// Audio: a page is served by one member, and silence every member holds
// is content served from the primary, without a wait.
func TestAudioPageStaysOnOneMemberAcrossSilence(t *testing.T) {
	const batch = 480
	a := NewAudio(2, 38, 2)
	a.Skew, a.Tolerance = 2048, ToleranceFor(40*time.Millisecond)
	s := newSamples(2, 2, 38)
	a.Zeros, a.Content = s.zeros, s.content
	now := time.Unix(0, 0)
	for b := uint64(10); b < 160; b++ {
		v := content
		if b >= 50 && b < 90 {
			v = zero // digital silence on every member
		}
		for m := range 2 {
			s.write(m, b*batch, (b+1)*batch, v)
			a.Committed(m, b*batch, (b+1)*batch)
		}
		now = now.Add(5 * time.Millisecond)
		before := a.Head
		sel, head, wait := a.Step(now)
		require.Zero(t, wait, "batch %d waited over silence every member holds", b)
		for _, p := range sel {
			require.Zero(t, p.Source, "batch %d moved page %d to %d", b, p.Page, p.Source)
		}
		for x := before; x < head; x++ {
			require.Zero(t, a.Current[(x/PageSamples)%a.Pages], "sample %d not from the primary", x)
		}
	}
	require.Equal(t, 0, a.Primary)
}
