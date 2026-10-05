package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one routed request, as logged and shown on the dashboard.
type Event struct {
	ID         int64      `json:"id"`
	Event      string     `json:"event"`
	Time       time.Time  `json:"time"`
	Client     string     `json:"client"`
	Path       string     `json:"path"`
	Model      string     `json:"model,omitempty"`
	Tools      int        `json:"tools"`
	Mode       string     `json:"mode"`
	Reason     string     `json:"reason,omitempty"`
	Tool       string     `json:"tool,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Status     int        `json:"status"`
	DurationMs int64      `json:"durationMs"`
	Usage      *llmUsage  `json:"usage,omitempty"`
	Jev        *jevRecord `json:"jev,omitempty"`
}

const (
	ringSize   = 2000
	rotateSize = 20 << 20
)

// Events appends events to a JSONL file (rotated to a single .1 backup),
// keeps the newest in memory, and fans them out to subscribers.
type Events struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	nextID int64
	ring   []Event
	subs   map[chan Event]struct{}
}

// OpenEvents loads the newest events of path and opens it for appending.
func OpenEvents(path string) (*Events, error) {
	e := &Events{path: path, subs: map[chan Event]struct{}{}, nextID: 1}
	if path == "" {
		return e, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	e.load(path + ".1")
	e.load(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	e.file, e.size = f, st.Size()
	return e, nil
}

func (e *Events) load(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev Event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Event != "route" {
			continue
		}
		e.remember(ev)
		if ev.ID >= e.nextID {
			e.nextID = ev.ID + 1
		}
	}
}

func (e *Events) remember(ev Event) {
	e.ring = append(e.ring, ev)
	if len(e.ring) > ringSize {
		e.ring = append(e.ring[:0:0], e.ring[len(e.ring)-ringSize:]...)
	}
}

// Add stamps ev with the next id, stores it and notifies subscribers. A log
// write failure loses the line, never the request.
func (e *Events) Add(ev Event) Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev.ID, ev.Event = e.nextID, "route"
	e.nextID++
	e.remember(ev)
	if e.file != nil {
		if line, err := json.Marshal(ev); err == nil {
			e.write(append(line, '\n'))
		}
	}
	for ch := range e.subs {
		select {
		case ch <- ev:
		default: // a slow dashboard drops events rather than stall requests
		}
	}
	return ev
}

func (e *Events) write(line []byte) {
	if e.size+int64(len(line)) > rotateSize {
		_ = e.file.Close()
		_ = os.Rename(e.path, e.path+".1")
		f, err := os.OpenFile(e.path, os.O_CREATE|os.O_TRUNC|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			e.file = nil
			return
		}
		e.file, e.size = f, 0
	}
	n, _ := e.file.Write(line)
	e.size += int64(n)
}

// Recent returns up to n of the newest events, oldest first.
func (e *Events) Recent(n int) []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	start := max(0, len(e.ring)-n)
	return append([]Event(nil), e.ring[start:]...)
}

// Subscribe returns a channel of new events and a function that ends the subscription.
func (e *Events) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	e.mu.Lock()
	e.subs[ch] = struct{}{}
	e.mu.Unlock()
	return ch, func() {
		e.mu.Lock()
		delete(e.subs, ch)
		e.mu.Unlock()
	}
}

// Close closes the log file.
func (e *Events) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.file == nil {
		return nil
	}
	err := e.file.Close()
	e.file = nil
	return err
}

var _ io.Closer = (*Events)(nil)
