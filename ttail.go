package ttail

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pkg/errors"
)

// FlagDebug enable debug output
var FlagDebug bool

type bufType struct {
	b         []byte
	lineStart int
	lineEnd   int
	discard   bool
}

func (b *bufType) reset() {
	b.lineStart = -1
	b.lineEnd = 0
	b.discard = true
}

// TFile represent file with sorted timestamps
// where binary search may be used
// currently this restriction not checked :-/
type TFile struct {
	opts     options
	file     *os.File
	fromTime time.Time
	offset   int64
	size     int64
	buf      bufType
}

// NewTimeFile create new time searcher configured by options
func NewTimeFile(f *os.File, opt ...TimeFileOptions) *TFile {
	tFileOptions := defaultOptions
	for _, o := range opt {
		o(&tFileOptions)
	}

	debug("NewTimeFile: with options %+v", tFileOptions)

	return &TFile{
		opts:     tFileOptions,
		file:     f,
		fromTime: time.Now(),
		buf:      bufType{b: make([]byte, tFileOptions.bufSize)},
	}

}

func debug(format string, args ...interface{}) {
	if FlagDebug {
		fmt.Fprintf(os.Stderr, ">>> "+format+"\n", args...)
	}
}

func (t *TFile) lastLineTime() (tm time.Time) {
	offset := t.offset - t.opts.bufSize
	if offset < 0 {
		offset = 0
	}

	for step := t.opts.stepsLimit; offset >= 0; offset -= t.opts.bufSize {
		if step--; step < 0 {
			debug("[lastLineTime]: attempts to read = %d, stop", t.opts.stepsLimit)
			return
		}
		count, err := t.file.ReadAt(t.buf.b, offset)
		if err != nil && err != io.EOF {
			debug("[lastLineTime]: read %s at %d: %s", t.file.Name(), offset, err)
			return
		}

		// begin search time from last line
		t.buf.lineEnd = 0
		t.buf.lineStart = count

		var line []byte
		for {
			t.buf.lineEnd = bytes.LastIndexByte(t.buf.b[:t.buf.lineStart], '\n')
			if t.buf.lineEnd == -1 {
				break
			}
			t.buf.lineStart = bytes.LastIndexByte(t.buf.b[:t.buf.lineEnd], '\n')
			if t.buf.lineStart == -1 {
				break
			} else if t.buf.lineStart > 0 {
				t.buf.lineStart++ // strip leader '\n'
			}

			line = t.buf.b[t.buf.lineStart:t.buf.lineEnd]
			debug("[lastLineTime]: search in: %q", line)

			if subm := t.opts.timeRe.FindSubmatch(line); subm != nil {
				debug("[lastLineTime]: regexp match for: %s", subm[1])
				tm, _ = time.ParseInLocation(t.opts.timeLayout, string(subm[1]), t.opts.location)
				debug("[lastLineTime]: found '%s' at %d", tm.Format(t.opts.timeLayout), offset)
				if !tm.IsZero() {
					t.offset = offset
					return tm
				}
			}
		}
		// if from origin of file left less then
		// t.opts.bufSize bytes read from origin
		if offset > 0 && offset < t.opts.bufSize {
			offset = t.opts.bufSize
		}
		debug("[lastLineTime]: offset=%d", offset)
	}
	return tm
}

func (t *TFile) readLine() ([]byte, error) {
	t.buf.b = t.buf.b[:t.opts.bufSize]
	// See comment in for loop
	t.buf.lineStart = -1
	// lineEnd must be zeroed
	t.buf.lineEnd = 0
	cursor := -1

	for {
		offset := t.offset + int64(t.buf.lineEnd)
		if cursor < 0 {
			// update actual last read file offset
			t.offset = offset
			debug("[readLine]: <for> read from %d", offset)
			n, err := t.file.ReadAt(t.buf.b[t.buf.lineEnd:], offset)
			debug("[readLine]: <for> read n=%d bytes (err = %v)", n, err)
			if err != nil {
				if err != io.EOF {
					return nil, errors.Wrap(err, "[readLine] <for> err")
				}
				if n <= 0 {
					return nil, err
				}
			}
			t.buf.b = t.buf.b[:t.buf.lineEnd+n]
			t.buf.discard = false
			if t.offset == 0 {
				t.buf.lineStart = 0
			}
		}

		cursor = bytes.IndexByte(t.buf.b[t.buf.lineEnd:], '\n')
		debug("[readLine]: <for> start=%d, cursor=%d", t.buf.lineStart, cursor)
		if cursor >= 0 {
			if t.buf.lineStart < 0 {
				// IndexByte use t.buf.lineEnd for speedup '\n' search
				// if initial buffer not contains this
				// next read extend buffer and IndexByte start from new bytes
				t.buf.lineStart = t.buf.lineEnd + cursor + 1
				t.buf.lineEnd = t.buf.lineStart
				continue
			}
			t.buf.lineEnd = t.buf.lineStart + cursor
			break
		}
		t.buf.lineEnd = len(t.buf.b)
		// '\n' not found and cursor is -1
		if int64(t.buf.lineEnd) >= t.opts.bufSize*4 {
			t.buf.lineStart = 0
			t.buf.lineEnd = 0
			break
		}

		// extend buffer
		t.buf.b = append(t.buf.b, make([]byte, t.opts.bufSize)...)
	}
	return t.buf.b[t.buf.lineStart:t.buf.lineEnd], nil
}

func (t *TFile) nextLine() ([]byte, error) {
	if t.buf.discard {
		// someone call buf.reset()
		return nil, io.EOF
	}

	t.buf.lineStart = t.buf.lineEnd + 1
	cursor := bytes.IndexByte(t.buf.b[t.buf.lineStart:], '\n')
	if cursor > 0 {
		t.buf.lineEnd = t.buf.lineStart + cursor
		return t.buf.b[t.buf.lineStart:t.buf.lineEnd], nil
	}
	return nil, io.EOF
}

func (t *TFile) findTime() (*time.Time, error) {
	var (
		line []byte
		err  error
		tm   time.Time
	)
	line, err = t.readLine()
	for err == nil {
		lineLen := len(line)
		if lineLen == 0 {
			debug("[findTime]: read junk continue from: %s", t.offset)
			t.offset += int64(t.buf.lineEnd)
			line, err = t.readLine()
		}
		debug("[findTime]: in: %s", line)

		if subm := t.opts.timeRe.FindSubmatch(line); subm != nil {
			debug("[findTime]: regexp match for: %s", subm[1])
			tm, err = time.ParseInLocation(t.opts.timeLayout, string(subm[1]), t.opts.location)
			if err == nil {
				return &tm, nil
			}
		} else {
			line = line[:0]
		}
	}
	if err != nil && err != io.EOF {
		err = errors.Wrap(err, "findTime")
	}
	return nil, err
}

func (t *TFile) preciseFindTime() error {
	var (
		line []byte
		err  error
		tm   time.Time
	)

	for err == nil {
		line, err = t.nextLine()
		if err == io.EOF {
			debug("[preciseFindTime]: got EOF")
			t.offset += int64(t.buf.lineEnd)
			line, err = t.readLine()
		}
		debug("[preciseFindTime]: nextLine[%d:%d] offset=%d", t.buf.lineStart, t.buf.lineEnd, t.offset)

		if subm := t.opts.timeRe.FindSubmatch(line); subm != nil {
			debug("[preciseFindTime]: parse as time: %s", subm[1])
			tm, err = time.ParseInLocation(t.opts.timeLayout, string(subm[1]), t.opts.location)
			if err != nil {
				debug("[preciseFindTime]: parse time error: %s", err)
				err = nil
				continue
			}
			if t.fromTime.Sub(tm) /* actual duration */ <= t.opts.duration {
				debug("[preciseFindTime]: found line: %s, offset=%d", tm, t.offset)
				break
			}
		}
	}
	return err
}

// FindPosition search file offset in log file
// where time is time.now() - <tail N seconds>
// or lastLineTime() - <tail N seconds>
func (t *TFile) FindPosition() error {
	var (
		at  *time.Time
		err error

		up     int64
		middle int64
		down   int64
	)

	down, err = t.file.Seek(0, os.SEEK_END)
	if err != nil {
		return err
	}
	if t.opts.timeFromLastLine {
		t.offset = down
		t.fromTime = t.lastLineTime()
		if t.fromTime.IsZero() {
			debug("[FindPosition]: time not found, copy whole file: %s", t.file.Name())
			t.offset = 0
			if err != nil {
				return err
			}
			return nil
		}
	}
	debug("[FindPosition]: Use fromTime: %s", t.fromTime.Format(t.opts.timeLayout))

	for (down - up) > t.opts.bufSize {
		middle = up + (down-up)/2 // avoid overflow middle
		t.offset = middle

		debug("[FindPosition]: BinSearch up=%d, down=%d, offset=%d", up, down, t.offset)
		for at = nil; at == nil; {
			at, err = t.findTime()
			if err != nil {
				return err
			}
		}

		if t.fromTime.Sub(*at) /* actual duration */ > t.opts.duration {
			up = middle
		} else {
			down = middle
		}
	}
	t.offset = up
	debug("[FindPosition]: found?(%s) up=%d, down=%d, offset=%d", at, up, down, t.offset)
	t.buf.reset()
	if err := t.preciseFindTime(); err != nil {
		return err
	}
	t.offset += int64(t.buf.lineStart)
	return nil
}

// CopyTo copies a file from the found
// through FindPosition offset to the end
func (t *TFile) CopyTo(w io.Writer) (int64, error) {
	_, _ = t.file.Seek(t.offset, os.SEEK_SET)
	debug("[CopyTo]: Copy file from offset=%d", t.offset)
	copied, err := io.Copy(w, t.file)
	if err != nil {
		debug("[CopyTo]: Copy only %d bytes: %s", copied, err)
	}
	return copied, err
}

// GetReader seek current file to target offset and return it
func (t *TFile) GetReader() (io.Reader, error) {
	_, err := t.file.Seek(t.offset, os.SEEK_SET)
	if err != nil {
		return nil, err
	}
	return t.file, nil
}
