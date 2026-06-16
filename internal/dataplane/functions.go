package dataplane

import (
	"bytes"
	"io"
	"net/http"
	"strconv"

	"github.com/mackee/localfront/internal/cffunc"
	"github.com/mackee/localfront/internal/config"
)

// functionErrorMessage is the body shown when a CloudFront Function fails.
// CloudFront answers function errors with a 503.
const functionErrorMessage = "The CloudFront Function associated with this distribution returned an error."

// lookup returns the compiled function for a behavior association, or nil.
func (snap *snapshot) lookup(fn *config.Function) *cffunc.Function {
	if snap.funcs == nil || fn == nil {
		return nil
	}
	return snap.funcs[fn.LogicalID]
}

func funcContext(dist *config.Distribution, requestID, eventType string) cffunc.Context {
	return cffunc.Context{
		DistributionDomainName: dist.DomainName,
		DistributionID:         dist.ID,
		EventType:              eventType,
		RequestID:              requestID,
	}
}

// runViewerRequest executes the viewer-request function. It returns
// (response, true) when the function short-circuits with a response,
// (nil, true) when it modified the request in place (continue to origin), and
// (nil, false) when it failed and a 503 was already written.
func (s *Server) runViewerRequest(w http.ResponseWriter, r *http.Request, dist *config.Distribution, beh *config.Behavior, snap *snapshot, viewerHeaders http.Header, requestID string) (*originResponse, bool) {
	fn := snap.lookup(beh.ViewerRequest)
	if fn == nil {
		s.logger.Error("viewer-request function is not compiled", "function", beh.ViewerRequest.LogicalID)
		writeCFError(w, http.StatusServiceUnavailable, requestID, functionErrorMessage)
		return nil, false
	}
	event := cffunc.NewRequestEvent("viewer-request", funcContext(dist, requestID, "viewer-request"), r, viewerHeaders)
	res, err := fn.Execute(event)
	if err != nil {
		s.logger.Error("viewer-request function failed", "function", beh.ViewerRequest.LogicalID, "error", err)
		writeCFError(w, http.StatusServiceUnavailable, requestID, functionErrorMessage)
		return nil, false
	}
	if res.IsResponse() {
		return functionResponse(res.Response), true
	}
	res.Request.ApplyToRequest(r)
	return nil, true
}

// runViewerResponse executes the viewer-response function, applying its result
// to resp in place. It returns false (after writing a 503) on failure.
func (s *Server) runViewerResponse(w http.ResponseWriter, r *http.Request, dist *config.Distribution, beh *config.Behavior, snap *snapshot, viewerHeaders http.Header, resp *originResponse, requestID string) bool {
	fn := snap.lookup(beh.ViewerResponse)
	if fn == nil {
		s.logger.Error("viewer-response function is not compiled", "function", beh.ViewerResponse.LogicalID)
		writeCFError(w, http.StatusServiceUnavailable, requestID, functionErrorMessage)
		return false
	}
	event := cffunc.NewRequestEvent("viewer-response", funcContext(dist, requestID, "viewer-response"), r, viewerHeaders)
	event.AttachResponse(resp.statusCode, resp.header)
	res, err := fn.Execute(event)
	if err != nil {
		s.logger.Error("viewer-response function failed", "function", beh.ViewerResponse.LogicalID, "error", err)
		writeCFError(w, http.StatusServiceUnavailable, requestID, functionErrorMessage)
		return false
	}
	if res.Response != nil {
		if res.Response.StatusCode != 0 {
			resp.statusCode = res.Response.StatusCode
		}
		resp.header = res.Response.HTTPHeaders()
	}
	return true
}

// functionResponse converts a function-produced response into a pipeline
// response so it flows through the response headers policy and writer.
func functionResponse(resp *cffunc.Response) *originResponse {
	header := resp.HTTPHeaders()
	body := resp.BodyBytes()
	if len(body) > 0 && header.Get("Content-Length") == "" {
		header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	return &originResponse{
		statusCode: status,
		header:     header,
		body:       io.NopCloser(bytes.NewReader(body)),
	}
}
