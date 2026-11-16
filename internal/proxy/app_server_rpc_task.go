package proxy

// requestIDValue returns a comparable string or int64, or nil for an invalid ID.
func requestIDValue(v rpcRaw) any {
	switch rpcKind(v) {
	case '"':
		var text string
		if rpcUnmarshal(v, &text) == nil {
			return text
		}
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		var number int64
		if rpcUnmarshal(v, &number) == nil {
			return number
		}
	}
	return nil
}

// beginIndependentRPC admits a completion-style RPC on its own work record. It
// never binds s.threads[thread], so the source's turn, review, prediction, and
// queue state are untouched even while the source turn keeps running.
func (s *appSession) beginIndependentRPC(message rpcMessage, method, thread string) error {
	if rpcLen(message["id"]) == 0 || rpcNull(message["id"]) {
		return s.rpcError(rpcValue(nil), -32600, "task methods require a request id")
	}
	work := &appWork{thread: thread, running: make(map[string]bool), completed: make(map[string]bool), rpcTask: true}
	s.works[work] = true
	if err := s.begin(work, message, method, "", 0); err != nil || work.pending == nil {
		delete(s.works, work)
		return err
	}
	return nil
}

// sampleCancel handles thread/sample/cancel. Before native has seen the sample
// the request is canceled locally; afterwards the client request id is mapped
// to the forwarded id. The cancel ACK only signals cancellation: it never
// releases the sample's own task slot.
func (s *appSession) sampleCancel(message rpcMessage, params rpcMessage) error {
	// An invalid request header is sent byte-for-byte so native sees the
	// original id. Request tracking would replace it with a valid proxy id.
	if requestIDValue(message["id"]) == nil {
		return writeRPC(s.stdin, message)
	}
	// ThreadSampleCancelParams denies unknown fields and accepts requestId only
	// as a string or i64. Invalid params keep the valid header so native's
	// error stays correlated with this request.
	if len(params) != 1 {
		return s.forward(message, "thread/sample/cancel", nil)
	}
	requestID := requestIDValue(params["requestId"])
	if requestID == nil {
		return s.forward(message, "thread/sample/cancel", nil)
	}
	for work := range s.works {
		job := work.pending
		if job == nil || job.method != "thread/sample" || requestIDValue(job.message["id"]) != requestID {
			continue
		}
		if err := s.cancelAdmission(work); err != nil {
			return err
		}
		return s.reply(message["id"], map[string]any{})
	}
	for nativeID, request := range s.requests {
		if request.method == "thread/sample" && requestIDValue(request.id) == requestID {
			// nativeID is the raw JSON id retained for the forwarded request.
			params["requestId"] = rpcInput([]byte(nativeID))
			message["params"] = rpcValue(params)
			break
		}
	}
	return s.forward(message, "thread/sample/cancel", nil)
}
