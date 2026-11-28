package modelgateway

import (
	"context"
	"errors"
	"testing"

	"github.com/nauticana/scout/domain"
)

func TestTableEndpointSourceReadsActiveRow(t *testing.T) {
	query := &catalogQueryFake{rows: map[string][][]any{
		qModelEndpoint: {{"O", " http://gpu-1:8000/v1 ", nil, int64(4)}},
	}}
	source := &TableEndpointSource{DB: catalogDBFake{query: query}}
	got, err := source.ModelEndpoint(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ModelEndpoint{ProviderID: "local", Protocol: domain.ModelProtocolOpenAI, BaseURL: "http://gpu-1:8000/v1", MaxConcurrency: 4}
	if got != want {
		t.Fatalf("endpoint = %+v, want %+v", got, want)
	}
	if args := query.args[qModelEndpoint]; len(args) != 1 || args[0] != "local" {
		t.Fatalf("query args = %v", args)
	}
	query.rows[qModelEndpoint] = nil
	if _, err := source.ModelEndpoint(context.Background(), "gone"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing row err = %v", err)
	}
}
