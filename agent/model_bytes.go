package agent

import (
	"io"
	"net/http"
	"sync"

	"github.com/neouks/series/runtimebudget"
)

type modelByteBudget struct {
	limits       runtimebudget.ModelByteLimits
	mu           sync.Mutex
	responseRead int64
}

// Only bytes actually read are counted. One extra byte is probed to distinguish
// exact-limit EOF from overflow; that byte is not delivered to SDK or capture.
// SDK attempts are serial. The mutex also bounds unusual concurrent reads that
// share a logical context, without reserving more than its remaining budget.
func (d *modelCallDeadline) readResponse(r io.Reader, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	d.bytes.mu.Lock()
	defer d.bytes.mu.Unlock()
	if err := d.ctx.Err(); err != nil {
		return 0, d.stopError(err)
	}
	limit := int64(d.bytes.limits.ResponseMiB) * runtimebudget.MiB
	left := max(0, limit-d.bytes.responseRead)
	readLen := min(int64(len(p)), left+1)
	n, err := r.Read(p[:int(readLen)])
	d.bytes.responseRead += int64(n)
	if int64(n) > left {
		d.cancel(&runtimebudget.ModelByteLimitError{Phase: "response", Limit: limit})
		return int(left), d.stopError(nil)
	}
	return n, err
}

func modelRequestByteLimit(req *http.Request) int64 {
	if owner, _ := req.Context().Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
		return int64(owner.bytes.limits.RequestMiB) * runtimebudget.MiB
	}
	return int64(runtimebudget.ModelBytePolicy(req.Context()).RequestMiB) * runtimebudget.MiB
}

func rejectModelHTTPSize(req *http.Request, limit int64) error {
	if req.Context().Err() != nil {
		return modelRequestError(req.Context(), req.Context().Err())
	}
	err := &runtimebudget.ModelByteLimitError{Phase: "request", Limit: limit}
	if owner, _ := req.Context().Value(modelDeadlineKey{}).(*modelCallDeadline); owner != nil {
		owner.cancel(err)
		return owner.stopError(err)
	}
	return err
}

// The final HTTP guard uses exact ContentLength. For chunked/custom callers,
// stop sending once the budget is exhausted; do not consume the whole body.
type boundedModelRequestBody struct {
	io.ReadCloser
	request  *http.Request
	left     int64
	limit    int64
	terminal error
}

func (b *boundedModelRequestBody) Read(p []byte) (int, error) {
	if b.terminal != nil {
		return 0, b.terminal
	}
	if len(p) == 0 {
		return 0, nil
	}
	n, err := b.ReadCloser.Read(p[:int(min(int64(len(p)), b.left+1))])
	if int64(n) > b.left {
		allowed := int(b.left)
		b.left = 0
		b.terminal = rejectModelHTTPSize(b.request, b.limit)
		return allowed, b.terminal
	}
	b.left -= int64(n)
	return n, err
}
