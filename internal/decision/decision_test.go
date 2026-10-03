package decision

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTemperatureRescalesChoice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{
			"topic":{"type":"choice","choice":"a","confidence":0.6,"probabilities":{"a":0.6,"b":0.3,"c":0.1}},
			"cx":{"type":"score","score":1.4,"confidence":0.5,"probabilities":{"0":0.2,"1":0.3,"2":0.5}}},
			"usage":{"input_tokens":3}}`))
	}))
	defer srv.Close()
	qs := map[string]Question{"topic": {Type: "choice"}, "cx": {Type: "score"}}

	p := NewHTTPProvider("x", srv.URL, "m", "", true, 100, time.Second)
	p.Temperature = 0.5
	res, err := p.Decide(context.Background(), map[string]string{"s": "hi"}, qs)
	if err != nil {
		t.Fatal(err)
	}
	topic := res.Answers["topic"]
	// 0.6², 0.3², 0.1² = .36, .09, .01 → a = .36/.46
	if topic.Choice != "a" || math.Abs(topic.Confidence-0.36/0.46) > 1e-9 || math.Abs(topic.Probabilities["c"]-0.01/0.46) > 1e-9 {
		t.Fatalf("choice not rescaled: %+v", topic)
	}
	if cx := res.Answers["cx"]; cx.Confidence != 0.5 || cx.Probabilities["2"] != 0.5 {
		t.Fatalf("score answer must be left alone: %+v", cx)
	}

	p.Temperature = 0
	res, _ = p.Decide(context.Background(), map[string]string{"s": "hi"}, qs)
	if res.Answers["topic"].Confidence != 0.6 {
		t.Fatalf("temperature 0 must be a no-op: %+v", res.Answers["topic"])
	}
}
