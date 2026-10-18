// Package ir encodes and decodes infrared remote-control frames. A frame
// travels as raw timings in microseconds, alternating mark (carrier on)
// and space, starting with a mark: that is what the screen sends and what
// its receiver reports (docs/protocol-v1.md §3.7, §4.6).
package ir

// Carrier is the modulation frequency of the codes here, in Hz.
const Carrier = 38000

// decoder walks raw timings.
type decoder struct {
	raw []int
	pos int
}

// near reports whether a measured duration matches the expected one:
// within a quarter, plus some slack for receivers that stretch marks and
// shorten spaces.
func near(got, want int) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= want/4+100
}

func (d *decoder) mark(want int) bool {
	if d.pos >= len(d.raw) || !near(d.raw[d.pos], want) {
		return false
	}
	d.pos++
	return true
}

func (d *decoder) pair(mark, space int) bool {
	if d.pos+1 >= len(d.raw) || !near(d.raw[d.pos], mark) || !near(d.raw[d.pos+1], space) {
		return false
	}
	d.pos += 2
	return true
}

// bits reads n bits, least significant first; each is a mark and a space
// whose length tells one from zero.
func (d *decoder) bits(n int, mark, one, zero int) (uint64, bool) {
	var v uint64
	for i := range n {
		switch {
		case d.pair(mark, one):
			v |= 1 << i
		case d.pair(mark, zero):
		default:
			return 0, false
		}
	}
	return v, true
}
