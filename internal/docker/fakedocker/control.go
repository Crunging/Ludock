package fakedocker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// control serves the /fake/ endpoints that journeys and tests use to change
// the daemon; faults never apply to them.
func (e *Engine) control(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/fake/fault":
		// Injects faults: POST /fake/fault?match=REGEXP with &status=CODE
		// fails, or &stall=1 stalls, the next &count=N requests whose
		// "METHOD path" matches (every one without a count); &clear=1 removes
		// all of them first.
		query := r.URL.Query()
		if query.Get("clear") != "" {
			e.ClearFaults()
		}
		if pattern := query.Get("match"); pattern != "" {
			status, _ := strconv.Atoi(query.Get("status"))
			count, _ := strconv.Atoi(query.Get("count"))
			if err := e.AddFault(pattern, status, query.Get("stall") != "", count); err != nil {
				fail(w, 400)
				return
			}
		}
		w.WriteHeader(204)
		return
	case "/fake/outage":
		// Goes away for a while, as a restarted daemon does:
		// POST /fake/outage?seconds=N answers, then the socket closes.
		seconds, _ := strconv.ParseFloat(r.URL.Query().Get("seconds"), 64)
		w.WriteHeader(204)
		go func() {
			time.Sleep(50 * time.Millisecond)
			e.Outage(time.Duration(seconds * float64(time.Second)))
		}()
		return
	case "/fake/requests":
		// Lists every Engine API request so far, one "METHOD path" a line.
		e.mu.Lock()
		requests := strings.Join(e.Requests, "\n")
		e.mu.Unlock()
		w.Write([]byte(requests))
		return
	case "/fake/rcon":
		// Counts the connections the fake game consoles have taken.
		fmt.Fprint(w, rconConnections.Load())
		return
	case "/fake/registry":
		// Lets journeys publish a new version of a tag or break its registry:
		// POST /fake/registry?image=REF&id=IMAGE-ID or &status=CODE.
		query := r.URL.Query()
		if id := query.Get("id"); id != "" {
			e.PublishImage(query.Get("image"), id)
		}
		if status := query.Get("status"); status != "" {
			code, _ := strconv.Atoi(status)
			e.FailRegistry(query.Get("image"), code)
		}
		w.WriteHeader(204)
		return
	case "/fake/disk":
		// Sets what measuring the disk under a host path reports:
		// POST /fake/disk?path=PATH&used=BYTES&available=BYTES, &fail=1 for a
		// failed measurement, or neither to measure for real.
		query := r.URL.Query()
		used, _ := strconv.ParseInt(query.Get("used"), 10, 64)
		available, _ := strconv.ParseInt(query.Get("available"), 10, 64)
		e.mu.Lock()
		switch {
		case query.Get("fail") != "":
			e.disks[query.Get("path")] = ""
		case query.Get("used") != "":
			encoded, _ := json.Marshal(map[string]int64{"total": used + available, "used": used, "available": available})
			e.disks[query.Get("path")] = string(encoded)
		default:
			delete(e.disks, query.Get("path"))
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/lose-upload":
		// Makes the next file write finish and then report an unknown result.
		e.mu.Lock()
		e.LoseUploads++
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/recreate":
		// Recreates a container with changed settings under a new ID, as
		// Compose or Portainer would: POST /fake/recreate?container=NAME
		// with any number of env=KEY=VALUE and label=KEY=VALUE; a KEY alone
		// removes it. image=REF changes the image reference it was created
		// from, keeping the image it runs. mount=DESTINATION=SOURCE moves the
		// bind mount at DESTINATION to another host folder. name=NAME renames it.
		query := r.URL.Query()
		e.Recreate(query.Get("container"), query)
		w.WriteHeader(204)
		return
	case "/fake/game":
		// Adds a fake game server like a real image's: POST
		// /fake/game?game=GAME&name=NAME. POST /fake/remove?container=NAME
		// removes it.
		query := r.URL.Query()
		if e.AddGame == nil || !e.AddGame(query.Get("game"), query.Get("name")) {
			fail(w, 404)
			return
		}
		w.WriteHeader(204)
		return
	case "/fake/start-after":
		// Starts a container as another manager would, right after the next
		// restore helper step: POST /fake/start-after?container=NAME&step=moveOld.
		query := r.URL.Query()
		e.mu.Lock()
		if e.startAfter == nil {
			e.startAfter = map[string]string{}
		}
		e.startAfter[query.Get("step")] = query.Get("container")
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	case "/fake/crashloop":
		// Leaves a container restarting, as Docker shows a game that keeps
		// exiting, until it is stopped: POST /fake/crashloop?container=NAME.
		e.mu.Lock()
		c := e.find(r.URL.Query().Get("container"))
		e.mu.Unlock()
		if c == nil {
			fail(w, 404)
			return
		}
		e.CrashLoop(c.ID)
		w.WriteHeader(204)
		return
	case "/fake/remove":
		e.mu.Lock()
		if c := e.find(r.URL.Query().Get("container")); c != nil {
			e.removeLocked(c.ID)
		}
		e.mu.Unlock()
		w.WriteHeader(204)
		return
	}
	fail(w, 404)
}

// Disk is the measurement a journey set for a host path: its JSON, or ""
// for a failure. ok is false when the path is measured for real.
func (e *Engine) Disk(path string) (result string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, ok = e.disks[path]
	return result, ok
}
