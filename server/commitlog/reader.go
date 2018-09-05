package commitlog

import (
	"errors"
	"io"
	"sync"

	"github.com/liftbridge-io/liftbridge/server/proto"

	"golang.org/x/net/context"
)

// ReadMessage reads a single message from the given Reader or blocks until one
// is available. It returns the Message in addition to its offset and
// timestamp. The headersBuf slice should have a capacity of at least 20.
func ReadMessage(reader io.Reader, headersBuf []byte) (Message, int64, int64, error) {
	if _, err := reader.Read(headersBuf); err != nil {
		return nil, 0, 0, err
	}
	var (
		offset    = int64(proto.Encoding.Uint64(headersBuf[0:]))
		timestamp = int64(proto.Encoding.Uint64(headersBuf[8:]))
		size      = proto.Encoding.Uint32(headersBuf[16:])
		buf       = make([]byte, int(size))
	)
	if _, err := reader.Read(buf); err != nil {
		return nil, 0, 0, err
	}
	return Message(buf), offset, timestamp, nil
}

type UncommittedReader struct {
	cl  *CommitLog
	seg *Segment
	mu  sync.Mutex
	pos int64
	ctx context.Context
}

func (r *UncommittedReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var (
		segments = r.cl.Segments()
		readSize int
		waiting  bool
	)

LOOP:
	for {
		readSize, err = r.seg.ReadAt(p[n:], r.pos)
		n += readSize
		r.pos += int64(readSize)
		if err != nil && err != io.EOF {
			break
		}
		if n == len(p) {
			break
		}
		if readSize != 0 && err == nil {
			waiting = false
			continue
		}

		// We hit the end of the segment.
		if err == io.EOF && !waiting {
			// Check if there are more segments.
			nextSeg := findSegmentByBaseOffset(segments, r.seg.BaseOffset+1)
			if nextSeg != nil {
				r.seg = nextSeg
				r.pos = 0
				continue
			}
			// Otherwise, wait for segment to be written to (or split).
			waiting = true
			if !r.waitForData(r.seg) {
				err = io.EOF
				break
			}
			continue
		}

		// If there are not enough segments to read, wait for new segment to be
		// appended or the context to be canceled.
		segments = r.cl.Segments()
		nextSeg := findSegmentByBaseOffset(segments, r.seg.BaseOffset+1)
		for nextSeg == nil {
			if !r.waitForData(r.seg) {
				err = io.EOF
				break LOOP
			}
			segments = r.cl.Segments()
		}
		r.seg = nextSeg
	}

	return n, err
}

func (r *UncommittedReader) waitForData(seg *Segment) bool {
	wait := seg.waitForData(r, r.pos)
	select {
	case <-r.cl.closed:
		seg.removeWaiter(r)
		return false
	case <-r.ctx.Done():
		seg.removeWaiter(r)
		return false
	case <-wait:
		return true
	}
}

// NewReaderUncommitted returns an io.Reader which reads data from the log
// starting at the given offset.
func (l *CommitLog) NewReaderUncommitted(ctx context.Context, offset int64) (io.Reader, error) {
	seg, _ := findSegment(l.Segments(), offset)
	if seg == nil {
		return nil, ErrSegmentNotFound
	}
	e, err := seg.findEntry(offset)
	if err != nil {
		return nil, err
	}
	return &UncommittedReader{
		cl:  l,
		seg: seg,
		pos: e.Position,
		ctx: ctx,
	}, nil
}

type CommittedReader struct {
	cl    *CommitLog
	seg   *Segment
	hwSeg *Segment
	mu    sync.Mutex
	pos   int64
	ctx   context.Context
	hwPos int64
	hw    int64
}

func (r *CommittedReader) Read(p []byte) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	segments := r.cl.Segments()

	// If seg is nil then the reader offset exceeded the HW, i.e. the log is
	// either empty or the offset overflows the HW. This means we need to wait
	// for data.
	if r.seg == nil {
		offset := r.hw + 1 // We want to read the next committed message.
		hw := r.cl.HighWatermark()
		for hw == r.hw {
			// The HW has not changed, so wait for it to update.
			if !r.waitForHW(hw) {
				err = io.EOF
				return
			}
			// Sync the HW.
			hw = r.cl.HighWatermark()
		}
		r.hw = hw
		segments = r.cl.Segments()
		hwIdx, hwPos, err := getHWPos(segments, r.hw)
		if err != nil {
			return 0, err
		}
		r.hwSeg = segments[hwIdx]
		r.hwPos = hwPos
		r.seg, _ = findSegment(segments, offset)
		if r.seg == nil {
			return 0, ErrSegmentNotFound
		}
		entry, err := r.seg.findEntry(offset)
		if err != nil {
			return 0, err
		}
		r.pos = entry.Position
	}

	return r.readLoop(p, segments)
}

func (r *CommittedReader) readLoop(p []byte, segments []*Segment) (n int, err error) {
	var readSize int
LOOP:
	for {
		lim := int64(len(p))
		if r.seg == r.hwSeg {
			// If we're reading from the HW segment, read up to the HW pos.
			lim = min(lim, r.hwPos-r.pos)
		}
		readSize, err = r.seg.ReadAt(p[n:lim], r.pos)
		n += readSize
		r.pos += int64(readSize)
		if err != nil && err != io.EOF {
			break
		}
		if n == len(p) {
			break
		}
		if readSize != 0 && err == nil {
			continue
		}

		// We hit the end of the segment, so jump to the next one.
		if err == io.EOF {
			nextSeg := findSegmentByBaseOffset(segments, r.seg.BaseOffset+1)
			if nextSeg == nil {
				// QUESTION: Should this ever happen?
				err = errors.New("no segment to consume")
				break
			}
			r.seg = nextSeg
			r.pos = 0
			continue
		}

		// We hit the HW, so sync the latest.
		hw := r.cl.HighWatermark()
		for hw == r.hw {
			// The HW has not changed, so wait for it to update.
			if !r.waitForHW(hw) {
				err = io.EOF
				break LOOP
			}
			// Sync the HW.
			hw = r.cl.HighWatermark()
		}
		r.hw = hw
		segments = r.cl.Segments()
		hwIdx, hwPos, err := getHWPos(segments, r.hw)
		if err != nil {
			break
		}
		r.hwPos = hwPos
		r.hwSeg = segments[hwIdx]
	}

	return n, err
}

func (r *CommittedReader) waitForHW(hw int64) bool {
	wait := r.cl.waitForHW(r, hw)
	select {
	case <-r.cl.closed:
		r.cl.removeHWWaiter(r)
		return false
	case <-r.ctx.Done():
		r.cl.removeHWWaiter(r)
		return false
	case <-wait:
		return true
	}
}

// NewReaderCommitted returns an io.Reader which reads only committed data from
// the log starting at the given offset.
func (l *CommitLog) NewReaderCommitted(ctx context.Context, offset int64) (io.Reader, error) {
	var (
		hw       = l.HighWatermark()
		hwPos    = int64(-1)
		segments = l.Segments()
		hwSeg    *Segment
		err      error
	)
	if hw != -1 {
		hwIdx, hwPosition, err := getHWPos(segments, hw)
		if err != nil {
			return nil, err
		}
		hwPos = hwPosition
		hwSeg = segments[hwIdx]
	}

	// If offset exceeds HW, wait for the next message. This also covers the
	// case when the log is empty.
	if offset > hw {
		return &CommittedReader{
			cl:    l,
			seg:   nil,
			pos:   -1,
			hwSeg: hwSeg,
			hwPos: hwPos,
			ctx:   ctx,
			hw:    hw,
		}, nil
	}

	if oldest := l.OldestOffset(); offset < oldest {
		offset = oldest
	}
	seg, _ := findSegment(segments, offset)
	if seg == nil {
		return nil, ErrSegmentNotFound
	}
	entry, err := seg.findEntry(offset)
	if err != nil {
		return nil, err
	}
	return &CommittedReader{
		cl:    l,
		seg:   seg,
		pos:   entry.Position,
		hwSeg: hwSeg,
		hwPos: hwPos,
		ctx:   ctx,
		hw:    hw,
	}, nil
}

func getHWPos(segments []*Segment, hw int64) (int, int64, error) {
	hwSeg, hwIdx := findSegment(segments, hw)
	if hwSeg == nil {
		return 0, 0, ErrSegmentNotFound
	}
	hwEntry, err := hwSeg.findEntry(hw)
	if err != nil {
		return 0, 0, err
	}
	return hwIdx, hwEntry.Position + int64(hwEntry.Size), nil
}

func min(x, y int64) int64 {
	if x < y {
		return x
	}
	return y
}
