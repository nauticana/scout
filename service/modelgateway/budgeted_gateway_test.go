package modelgateway

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/nauticana/scout/contract"
	"github.com/nauticana/scout/domain"
	"github.com/nauticana/scout/fake"
)

func budgetedRequest() domain.ModelRequest {
	request := validModelRequest()
	request.Prompt = []byte("hello")
	request.Search = &domain.SearchGrounding{MaxSearches: 3}
	return request
}

func TestBudgetedGatewayReservesBeforeTheCallAndSettlesConfirmedUsage(t *testing.T) {
	recorder := newBudgetRecorder()
	var reserved domain.BudgetRequest
	manager := recorder.manager()
	reserve := manager.ReserveFunc
	manager.ReserveFunc = func(ctx context.Context, request domain.BudgetRequest) (domain.BudgetReservation, error) {
		reserved = request
		return reserve(ctx, request)
	}
	var priced domain.ModelUsage
	pricer := fake.ModelPricerFunc(func(_ context.Context, _ domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
		priced = usage
		return 20, "USD", nil
	})
	usage := domain.Usage{InputTokens: 4, OutputTokens: 11, CostMinorUnits: 9, Currency: "USD"}
	var order []string
	inner := &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
		order = append(order, "generate")
		return domain.ModelResult{Output: []byte("ok"), Usage: usage}, nil
	}}
	gateway, err := NewBudgetedGateway(inner, manager, pricer)
	if err != nil {
		t.Fatalf("NewBudgetedGateway: %v", err)
	}
	result, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest())
	if err != nil || string(result.Output) != "ok" {
		t.Fatalf("Generate = %+v, %v", result, err)
	}
	if priced.SearchQueries != 3 || priced.OutputTokens != 100 {
		t.Fatalf("the reservation must cover the output ceiling and the grounding searches, priced = %+v", priced)
	}
	if reserved.RequestID != "request" || reserved.Tokens != promptTokens(nil, []byte("hello"))+100 || reserved.CostMinorUnits != 20 {
		t.Fatalf("reserved = %+v", reserved)
	}
	reservations, settled, released := recorder.snapshot()
	if len(reservations) != 1 || len(released) != 0 || settled["request"] != usage {
		t.Fatalf("reserved %v, settled %v, released %v", reservations, settled, released)
	}
	if len(order) != 1 {
		t.Fatalf("the call must run once, order = %v", order)
	}
}

func TestBudgetedGatewayReleasesWhenTheCallSpendsNothing(t *testing.T) {
	recorder := newBudgetRecorder()
	broken := errors.New("provider down")
	gateway := &BudgetedGateway{
		Inner: &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{}, broken
		}},
		Budgets: recorder.manager(),
		Pricer:  fixedPricer(),
	}
	if _, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest()); !errors.Is(err, broken) {
		t.Fatalf("Generate error = %v", err)
	}
	_, settled, released := recorder.snapshot()
	if len(settled) != 0 || len(released) != 1 {
		t.Fatalf("a failed call releases its hold: settled %v, released %v", settled, released)
	}
}

func TestBudgetedGatewayPricesConfirmedProviderUsage(t *testing.T) {
	recorder := newBudgetRecorder()
	var priced []domain.ModelUsage
	pricer := fake.ModelPricerFunc(func(_ context.Context, _ domain.ModelReference, usage domain.ModelUsage) (int64, string, error) {
		priced = append(priced, usage)
		return 13, "USD", nil
	})
	gateway := &BudgetedGateway{
		Inner: &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{Usage: domain.Usage{InputTokens: 7, OutputTokens: 3, SearchQueries: 2}}, nil
		}},
		Budgets: recorder.manager(), Pricer: pricer,
	}
	if _, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest()); err != nil {
		t.Fatal(err)
	}
	if len(priced) != 2 || priced[1].InputTokens != 7 || priced[1].OutputTokens != 3 || priced[1].SearchQueries != 2 {
		t.Fatalf("priced usage = %+v", priced)
	}
	_, settled, _ := recorder.snapshot()
	usage := settled["request"]
	if usage.CostMinorUnits != 13 || usage.Currency != "USD" {
		t.Fatalf("settled usage = %+v", usage)
	}
}

func TestBudgetedGatewayRefusesTheCallWhenTheBudgetIsExhausted(t *testing.T) {
	recorder := newBudgetRecorder()
	recorder.failFor = "request"
	called := false
	gateway := &BudgetedGateway{
		Inner: &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			called = true
			return domain.ModelResult{}, nil
		}},
		Budgets: recorder.manager(),
		Pricer:  fixedPricer(),
	}
	if _, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest()); !errors.Is(err, domain.ErrBudgetExceeded) {
		t.Fatalf("Generate error = %v", err)
	}
	if called {
		t.Fatal("a refused reservation must not reach the model")
	}
}

func TestBudgetedGatewayStreamSettlesOnceWhenTheStreamFinishes(t *testing.T) {
	recorder := newBudgetRecorder()
	usage := domain.Usage{InputTokens: 2, OutputTokens: 5, CostMinorUnits: 3, Currency: "USD"}
	frames := []domain.ModelChunk{
		{Sequence: 1, Payload: []byte("a")},
		{Sequence: 2, Payload: []byte("b"), FinishReason: "stop", Usage: usage},
	}
	gateway := &BudgetedGateway{
		Inner: &fake.ModelGateway{StreamFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (contract.ModelStream, error) {
			return &sliceStream{frames: frames}, nil
		}},
		Budgets: recorder.manager(),
		Pricer:  fixedPricer(),
	}
	stream, err := gateway.Stream(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range frames {
		if _, err := stream.Receive(context.Background()); err != nil {
			t.Fatalf("Receive: %v", err)
		}
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, settled, released := recorder.snapshot()
	if len(released) != 0 || settled["request"] != usage {
		t.Fatalf("the stream settles its own confirmed usage exactly once: settled %v, released %v", settled, released)
	}
}

// sliceStream replays fixed frames and then ends.
type sliceStream struct {
	frames []domain.ModelChunk
	next   int
}

func (stream *sliceStream) Receive(context.Context) (domain.ModelChunk, error) {
	if stream.next >= len(stream.frames) {
		return domain.ModelChunk{}, io.EOF
	}
	frame := stream.frames[stream.next]
	stream.next++
	return frame, nil
}

func (stream *sliceStream) Close() error { return nil }

func TestBudgetedGatewayClosesTheHoldWhenConfirmedUsageCannotBePriced(t *testing.T) {
	recorder := newBudgetRecorder()
	unpriced := errors.New("no price")
	calls := 0
	pricer := fake.ModelPricerFunc(func(context.Context, domain.ModelReference, domain.ModelUsage) (int64, string, error) {
		if calls++; calls > 1 {
			return 0, "", unpriced
		}
		return 20, "USD", nil
	})
	gateway := &BudgetedGateway{
		Inner: &fake.ModelGateway{GenerateFunc: func(context.Context, domain.ModelSelection, domain.ModelRequest) (domain.ModelResult, error) {
			return domain.ModelResult{Usage: domain.Usage{InputTokens: 7, OutputTokens: 3}}, nil
		}},
		Budgets: recorder.manager(), Pricer: pricer,
	}
	if _, err := gateway.Generate(context.Background(), domain.ModelSelection{Provider: "openai", Model: "gpt"}, budgetedRequest()); !errors.Is(err, unpriced) {
		t.Fatalf("the pricing failure must surface, err = %v", err)
	}
	if _, settled, _ := recorder.snapshot(); settled["request"].InputTokens != 7 {
		t.Fatalf("the hold must be closed against the confirmed tokens, settled = %v", settled)
	}
}
