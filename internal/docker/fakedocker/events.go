package fakedocker

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

func (e *Engine) emitLocked(action, id string) {
	// Docker sends the container's labels and name as actor attributes.
	attributes := map[string]string{}
	if c := e.containers[id]; c != nil {
		for key, value := range c.Config.Labels {
			attributes[key] = value
		}
		attributes["name"] = strings.TrimPrefix(c.Name, "/")
	}
	event, _ := json.Marshal(map[string]any{"Type": "container", "Action": action,
		"Actor": map[string]any{"ID": id, "Attributes": attributes}, "id": id, "time": time.Now().Unix()})
	event = append(event, '\n')
	for _, events := range e.events {
		select {
		case events <- event:
		default:
		}
	}
}

func (e *Engine) streamEvents(w http.ResponseWriter, r *http.Request) {
	events := make(chan []byte, 256)
	e.mu.Lock()
	e.events = append(e.events, events)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		for index, existing := range e.events {
			if existing == events {
				e.events = append(e.events[:index], e.events[index+1:]...)
				break
			}
		}
		e.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			w.Write(event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}
