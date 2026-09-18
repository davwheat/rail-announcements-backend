package hls

import "sync"

// listenerBuffer is how many frames a listener may fall behind: about six seconds.
const listenerBuffer = 256

// Broadcast hands every frame to listeners who take the stream as one endless
// response, the way internet radio is served.
type Broadcast struct {
	mu        sync.Mutex
	keep      int
	recent    [][]byte
	listeners map[chan []byte]struct{}
	closed    bool
}

// NewBroadcast keeps the last keep frames to open each new response with.
func NewBroadcast(keep int) *Broadcast {
	return &Broadcast{keep: keep, listeners: map[chan []byte]struct{}{}}
}

func (b *Broadcast) Add(frame []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, frame)
	if extra := len(b.recent) - b.keep; extra > 0 {
		b.recent = b.recent[extra:]
	}
	for listener := range b.listeners {
		select {
		case listener <- frame:
		default:
			// A listener this far behind is no longer hearing announcements on
			// time. Closing it makes its player reconnect at the live edge.
			delete(b.listeners, listener)
			close(listener)
		}
	}
}

// Listen returns the recent past, which leads seamlessly into the first frame
// delivered, and every frame from now on. The channel closes when the
// broadcast ends or the listener falls too far behind. Call stop when done.
func (b *Broadcast) Listen() (recent []byte, frames <-chan []byte, stop func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, frame := range b.recent {
		recent = append(recent, frame...)
	}
	listener := make(chan []byte, listenerBuffer)
	if b.closed {
		close(listener)
		return recent, listener, func() {}
	}
	b.listeners[listener] = struct{}{}
	return recent, listener, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, listening := b.listeners[listener]; listening {
			delete(b.listeners, listener)
			close(listener)
		}
	}
}

// Close ends every listener's response. Call it when no more frames will come.
func (b *Broadcast) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for listener := range b.listeners {
		delete(b.listeners, listener)
		close(listener)
	}
}
