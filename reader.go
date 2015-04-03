package limio

import (
	"io"
	"math"
	"sync"
	"time"
)

const (
	window  = 100 * time.Microsecond
	bufsize = KB << 3
)

//A Limiter gets some stream of things. It only lets N things through per T time.
//It can either be given both T and N or block and wait for N on a channel.
//In this case, T is handled externally to the limiter.
//
//Most simply, underlying implementation should always use a channel. It can be
//replaced with a new channel directly or by providing N and T at which point
//an internal channel is set up.
type Limiter interface {
	Limit(n uint64, t time.Duration)
	LimitChan(<-chan uint64)
}

//LimitWaiter is a limiter that also exposes the ability to know when the
//underlying data has been completed. Wait() in this context is identical to
//sync.WaitGroup.Wait() (and is most likely implemented with a sync.WaitGroup)
//in that it blocks until it is freed by the underlying implementation.
type LimitWaiter interface {
	Limiter
	Wait()
}

type Reader struct {
	r      io.Reader
	buf    []byte
	eof    bool
	done   *sync.WaitGroup
	remain uint64

	rMut sync.RWMutex
	rate <-chan uint64
}

func (r *Reader) rater() <-chan uint64 {
	r.rMut.RLock()
	defer r.rMut.RUnlock()
	return r.rate
}

func (r *Reader) Read(p []byte) (written int, err error) {
	if r.r == nil {
		err = io.ErrUnexpectedEOF
		return
	}
	if r.eof {
		err = io.EOF
		return
	}

	if r.buf == nil {
		r.buf = make([]byte, bufsize)
	}

	for written < len(p) {
		var lim uint64
		if r.rater() != nil {
			if r.remain == 0 {
				select {
				case r.remain = <-r.rater():
					break
				default:

					if written > 0 {
						return
					}
					r.remain = <-r.rater()
				}
			}

			lim = r.remain
		}

		if lim == 0 {
			lim -= 1
		}

		if lim > uint64(len(p[written:])) {
			lim = uint64(len(p[written:]))
		}

		var n int
		n, err = r.r.Read(r.buf[:lim])

		copy(p[written:], r.buf[:n])
		written += n

		if r.rater() != nil {
			r.remain -= uint64(n)
		}

		if err != nil {
			if err == io.EOF {
				r.eof = true
				r.done.Done()
			}

			return
		}
	}
	return
}

//Limit provides a basic means for limiting a Reader. Given n bytes per t
//time, it does its best to maintain a constant rate with a high degree of
//accuracy to allow other algorithms (such as TCP window sizing, e.g.) to
//self-adjust.
//
//It is safe to assume that Reader.Limit can be called concurrently with a Read
//operation, though the Read operation will continue to use the prior rate until
//it is requests a rate update.
func (r *Reader) Limit(n uint64, t time.Duration) {
	ratio := float64(t) / float64(window)
	nPer := float64(n) / ratio
	n = uint64(nPer)

	if nPer < 1.0 {
		t = time.Duration(math.Pow(nPer, -1))
		n = 1
	} else {
		t = window
	}

	//TODO make sure no memory leaks
	ch := make(chan uint64)

	r.rMut.Lock()
	r.rate = ch
	r.rMut.Unlock()

	tkr := time.NewTicker(t)
	go func() {
		for _ = range tkr.C {
			if r.eof {
				return
			}

			ch <- n
		}
	}()
}

func (r *Reader) LimitChan(c <-chan uint64) {
	r.rMut.Lock()
	r.rate = c
	r.rMut.Unlock()
}

//Close will block until eof is reached. Once reached, any errors will be
//returned. It is intended to provide synchronization for external channel
//managers
func (r *Reader) Wait() {
	if !r.eof {
		r.done.Wait()
	}
	return
}

//NewReader takes an io.Reader and returns a Limitable Reader.
func NewReader(r io.Reader) *Reader {
	switch r := r.(type) {
	case *Reader:
		return r
	default:
		nr := Reader{
			r:    r,
			buf:  make([]byte, bufsize),
			done: &sync.WaitGroup{},
			rMut: sync.RWMutex{},
		}

		nr.done.Add(1)

		return &nr
	}
}
