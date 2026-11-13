package standin

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// Plainforge is an in-memory stand-in for the one thing Plainforge does to
// Keelworks in this milestone: deliver a push notification, in the format of
// contract/v1/push.md, with the bearer token of its ServiceAccount.
type Plainforge struct {
	// Keelworks is the handler notifications are delivered to.
	Keelworks http.Handler
	// Token is the bearer token Plainforge presents.
	Token string
}

// Notification builds the body Plainforge sends for a push that moved ref
// from the commit before to the commit after.
func Notification(repository, ref, before, after string) []byte {
	body, _ := json.Marshal(struct {
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		Before     string `json:"before"`
		After      string `json:"after"`
	}{repository, ref, before, after})
	return body
}

// Push delivers a notification and returns Keelworks' status and answer.
func (p Plainforge) Push(repository, ref, before, after string) (int, []byte) {
	return p.Deliver(Notification(repository, ref, before, after))
}

// Deliver posts a raw body to the webhook path with Plainforge's token.
func (p Plainforge) Deliver(body []byte) (int, []byte) {
	status, _, answer := Call(p.Keelworks, []string{"Bearer " + p.Token}, body)
	return status, answer
}

// Call posts a raw body to the webhook path, in memory, with exactly the
// given Authorization header values. It is how a caller that is not
// Plainforge is played.
func Call(keelworks http.Handler, authorization []string, body []byte) (int, http.Header, []byte) {
	req, err := http.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	if err != nil {
		panic("standin: fixed request cannot be built") // unreachable
	}
	req.Header.Set("Content-Type", "application/json")
	for _, value := range authorization {
		req.Header.Add("Authorization", value)
	}
	rec := &recorder{header: http.Header{}, status: http.StatusOK}
	keelworks.ServeHTTP(rec, req)
	return rec.status, rec.header, rec.body.Bytes()
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
