package memfile

import (
	"io"
)

type FileLike interface {
	io.Writer
	io.Seeker
	io.Reader
}

func New() FileLike {
	return &memfile{}
}

type memfile struct {
	off int64
	len int64
	b   []byte
}

// grow ensures the backing slice has room for n bytes starting at the current
// offset. The returned slice aliases mf.b up to the new logical length.
func (mf *memfile) grow(end int64) {
	if end <= int64(cap(mf.b)) {
		if end > int64(len(mf.b)) {
			mf.b = mf.b[:end]
		}
		return
	}

	newCap := int64(cap(mf.b)) * 2
	if newCap < end {
		newCap = end
	}
	if newCap < 4096 {
		newCap = 4096
	}

	nb := make([]byte, end, newCap)
	copy(nb, mf.b)
	mf.b = nb
}

func (mf *memfile) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	end := mf.off + int64(len(p))
	mf.grow(end)

	n := copy(mf.b[mf.off:end], p)
	mf.off += int64(n)
	if mf.len < mf.off {
		mf.len = mf.off
	}
	return n, nil
}

func (mf *memfile) Read(p []byte) (int, error) {
	if mf.off >= mf.len {
		return 0, io.EOF
	}

	n := copy(p, mf.b[mf.off:mf.len])
	mf.off += int64(n)
	return n, nil
}

func (mf *memfile) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		mf.off = off
	case io.SeekEnd:
		// Preserved historical behavior: off is a positive distance back from
		// the logical end (callers pass 0 for an end query).
		mf.off = mf.len - off
	case io.SeekCurrent:
		mf.off += off
	}
	if mf.off < 0 {
		mf.off = 0
	}
	if mf.off > mf.len {
		mf.off = mf.len
	}
	return mf.off, nil
}
