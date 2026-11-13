package standin

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// Plainforge is an in-memory stand-in for the one thing Plainforge does to
// Keelworks in this milestone: deliver a push notification.
//
// SPEC.md does not define the notification. The stand-in sends the smallest
// body that carries what the intake rules need: a JSON object with exactly
// the string fields "repository", "ref", "commit" and "message".
type Plainforge struct {
	// Keelworks is the handler notifications are delivered to.
	Keelworks http.Handler
}

// Notification builds the body Plainforge sends for a push.
func Notification(repository, ref, commit, message string) []byte {
	body, _ := json.Marshal(struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Commit     string `json:"commit"`
		Message    string `json:"message"`
	}{repository, ref, commit, message})
	return body
}

// Push delivers a notification and returns Keelworks' status and answer.
func (p Plainforge) Push(repository, ref, commit, message string) (int, []byte) {
	return p.Deliver(Notification(repository, ref, commit, message))
}

// Deliver posts a raw body to the webhook path, in memory.
func (p Plainforge) Deliver(body []byte) (int, []byte) {
	req, err := http.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if err != nil {
		panic("standin: fixed request cannot be built") // unreachable
	}
	req.Header.Set("Content-Type", "application/json")
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	p.Keelworks.ServeHTTP(rec, req)
	return rec.status, rec.body.Bytes()
}

// recorder is a minimal in-memory http.ResponseWriter.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) WriteHeader(status int)      { r.status = status }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
