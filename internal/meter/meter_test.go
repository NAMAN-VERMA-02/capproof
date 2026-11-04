package meter

import "testing"

func TestInspect(t *testing.T) {
	tests := []struct {
		name, body string
		want       int64
		wantErr    bool
	}{
		{name: "plain", body: `{"max_tokens":5}`, want: 5},
		{name: "new name", body: `{"max_completion_tokens":7}`, want: 7},
		{name: "both take the larger", body: `{"max_tokens":1,"max_completion_tokens":9000}`, want: 9000},
		{name: "n of one", body: `{"max_tokens":5,"n":1}`, want: 5},
		{name: "nested does not count", body: `{"metadata":{"max_tokens":1},"max_tokens":5}`, want: 5},
		{name: "standard tier", body: `{"max_tokens":5,"service_tier":"auto"}`, want: 5},
		{name: "duplicate limit", body: `{"max_tokens":9000,"max_tokens":1}`, wantErr: true},
		{name: "case variant", body: `{"max_tokens":9000,"MAX_TOKENS":1}`, wantErr: true},
		{name: "case variant alone", body: `{"Max_Tokens":1}`, wantErr: true},
		{name: "duplicate model", body: `{"max_tokens":5,"model":"cheap","model":"costly"}`, wantErr: true},
		{name: "case variant model", body: `{"max_tokens":5,"model":"cheap","MODEL":"costly"}`, wantErr: true},
		{name: "n above one", body: `{"max_tokens":5,"n":100}`, wantErr: true},
		{name: "priority tier", body: `{"max_tokens":5,"service_tier":"priority"}`, wantErr: true},
		{name: "fast mode", body: `{"max_tokens":5,"speed":"fast"}`, wantErr: true},
		{name: "regional pricing", body: `{"max_tokens":5,"inference_geo":"us"}`, wantErr: true},
		{name: "fractional", body: `{"max_tokens":1.5}`, wantErr: true},
		{name: "string", body: `{"max_tokens":"5"}`, wantErr: true},
		{name: "null", body: `{"max_tokens":null}`, wantErr: true},
		{name: "zero", body: `{"max_tokens":0}`, wantErr: true},
		{name: "negative", body: `{"max_tokens":-1}`, wantErr: true},
		{name: "missing", body: `{"model":"m"}`, wantErr: true},
		{name: "array", body: `[{"max_tokens":5}]`, wantErr: true},
		{name: "truncated", body: `{"max_tokens":5`, wantErr: true},
		{name: "empty", body: ``, wantErr: true},
		{name: "second document", body: `{"max_tokens":5}{"max_tokens":9}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Inspect([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %t", err, tt.wantErr)
			}
			if err == nil && got.MaxOutput != tt.want {
				t.Fatalf("limit %d, want %d", got.MaxOutput, tt.want)
			}
		})
	}
}

func TestInspectReadsStreamAndModel(t *testing.T) {
	got, err := Inspect([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"max_tokens":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || !got.Stream || !got.HasUsageOption {
		t.Fatalf("%+v", got)
	}
}

func TestTracker(t *testing.T) {
	rates := Rates{Input: 2, Output: 3, CacheWrite: 5, CacheRead: 7}
	tests := []struct {
		name  string
		lines []string
		want  int64
		known bool
	}{
		{"openai", []string{`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`}, 35, true},
		{"anthropic keeps the last value", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"message_delta","usage":{"output_tokens":3}}`,
			`{"type":"message_delta","usage":{"output_tokens":5}}`,
		}, 35, true},
		{"input can grow within a stream", []string{
			`{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
			`{"type":"message_delta","usage":{"input_tokens":40,"output_tokens":5}}`,
		}, 95, true},
		// Anthropic counts cached tokens apart from input_tokens, so ignoring them undercounts.
		{"cache tokens are billed", []string{
			`{"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":10,"cache_read_input_tokens":100}}`,
		}, 2 + 3 + 50 + 700, true},
		{"nothing", []string{`{"choices":[]}`, `[DONE]`, ``}, 0, false},
		{"null usage", []string{`{"usage":null}`}, 0, false},
		{"input only", []string{`{"usage":{"prompt_tokens":10}}`}, 0, false},
		{"wrong types", []string{`{"usage":{"prompt_tokens":"oops","completion_tokens":null}}`}, 0, false},
		{"negative", []string{`{"usage":{"prompt_tokens":-1,"completion_tokens":1}}`}, 0, false},
		{"negative cache", []string{`{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":-9}}`}, 0, false},
		{"good then malformed stays unknown", []string{
			`{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			`{"usage":{"prompt_tokens":"oops"}}`,
		}, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := Tracker{Rates: rates}
			for _, line := range tt.lines {
				tr.Feed([]byte(line))
			}
			got, known := tr.Cost()
			if int64(got) != tt.want || known != tt.known {
				t.Fatalf("cost = %d, %t; want %d, %t", got, known, tt.want, tt.known)
			}
		})
	}
}

func TestTrackerFinished(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{{`[DONE]`, true}, {`{"type":"message_stop"}`, true}, {`{"type":"message_delta"}`, false}} {
		tr := Tracker{}
		tr.Feed([]byte(tt.line))
		if tr.Finished() != tt.want {
			t.Errorf("%s: finished = %t", tt.line, tr.Finished())
		}
	}
}
