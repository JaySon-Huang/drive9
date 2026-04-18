package backend

import "testing"

func TestIsDirectTextSemanticCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		contentType string
		want        bool
	}{
		{name: "text plain", path: "/docs/a.txt", contentType: "text/plain", want: true},
		{name: "text with params", path: "/docs/a.txt", contentType: "text/markdown; charset=utf-8", want: true},
		{name: "json", path: "/docs/a.json", contentType: "application/json", want: true},
		{name: "xml", path: "/docs/a.xml", contentType: "application/xml", want: true},
		{name: "yaml", path: "/docs/a.yaml", contentType: "application/yaml", want: true},
		{name: "octet stream deferred", path: "/code/main.go", contentType: "application/octet-stream", want: false},
		{name: "image", path: "/img/a.png", contentType: "image/png", want: false},
		{name: "audio", path: "/audio/a.mp3", contentType: "audio/mpeg", want: false},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isDirectTextSemanticCandidate(tc.path, tc.contentType); got != tc.want {
				t.Fatalf("isDirectTextSemanticCandidate(%q, %q)=%v, want %v", tc.path, tc.contentType, got, tc.want)
			}
		})
	}
}

func TestShouldEnqueueFileSemanticTask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		contentType string
		size        int64
		contentText string
		want        bool
	}{
		{
			name:        "large direct text",
			path:        "/docs/large.txt",
			contentType: "text/plain",
			size:        smallFileThreshold + 1,
			contentText: "",
			want:        true,
		},
		{
			name:        "small sync insufficient direct text",
			path:        "/docs/small.txt",
			contentType: "text/plain",
			size:        128,
			contentText: "",
			want:        true,
		},
		{
			name:        "small sync sufficient direct text",
			path:        "/docs/small.txt",
			contentType: "text/plain",
			size:        128,
			contentText: "hello world",
			want:        false,
		},
		{
			name:        "zero byte file stays on sync path",
			path:        "/docs/empty.txt",
			contentType: "text/plain",
			size:        0,
			contentText: "",
			want:        false,
		},
		{
			name:        "unsupported large file",
			path:        "/bin/blob.bin",
			contentType: "application/octet-stream",
			size:        smallFileThreshold + 1,
			contentText: "",
			want:        false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldEnqueueFileSemanticTask(tc.path, tc.contentType, tc.size, tc.contentText); got != tc.want {
				t.Fatalf("shouldEnqueueFileSemanticTask(%q, %q, %d, %q)=%v, want %v", tc.path, tc.contentType, tc.size, tc.contentText, got, tc.want)
			}
		})
	}
}
