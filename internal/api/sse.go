package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// streamSSE handles subscribing to a stream, replaying optional history, deduping by sequence,
// and flushing SSE frames until client disconnect or channel close.
func streamSSE[T any](
	writer http.ResponseWriter,
	request *http.Request,
	getHistory func() []T,
	subscribe func() (<-chan T, func()),
	getSeq func(T) uint64,
) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		http.Error(writer, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch, cancel := subscribe()
	defer cancel()

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()

	var maxSeq uint64
	emit := func(entry T) {
		seq := getSeq(entry)
		if seq <= maxSeq {
			return
		}
		maxSeq = seq
		data, err := json.Marshal(entry)
		if err != nil {
			return
		}
		fmt.Fprintf(writer, "data: %s\n\n", data)
	}

	if request.URL.Query().Get("history") == "true" && getHistory != nil {
		for _, entry := range getHistory() {
			emit(entry)
		}
		flusher.Flush()
	}

	ctx := request.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			emit(entry)
			flusher.Flush()
		}
	}
}
