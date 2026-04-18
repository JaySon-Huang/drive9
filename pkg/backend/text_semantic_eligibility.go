package backend

import "strings"

var directTextSemanticMIMETypes = map[string]struct{}{
	"application/json": {},
	"application/xml":  {},
	"application/yaml": {},
}

func isDirectTextSemanticCandidate(path, contentType string) bool {
	// Keep path in the signature so Phase 1 can add an extension allowlist in a
	// follow-up without churning write-path call sites.
	_ = path

	contentType = stripMIMEParams(contentType)
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	_, ok := directTextSemanticMIMETypes[contentType]
	return ok
}

func shouldEnqueueFileSemanticTask(path, contentType string, size int64, contentText string) bool {
	if !isDirectTextSemanticCandidate(path, contentType) {
		return false
	}
	if size > smallFileThreshold {
		return true
	}
	if size <= 0 {
		return false
	}
	return strings.TrimSpace(contentText) == ""
}
