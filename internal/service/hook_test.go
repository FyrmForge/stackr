package service_test

import "net/http"

// hook is the headers of a GitHub delivery of event signed sig, as the
// hooks route hands them to Webhook.
func hook(event, sig string) http.Header {
	return http.Header{"X-Github-Event": {event}, "X-Hub-Signature-256": {sig}}
}
