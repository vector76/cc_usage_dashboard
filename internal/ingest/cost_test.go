package ingest

import (
	"testing"
)

func TestResolveCostReported(t *testing.T) {
	reportedCost := 0.05
	cost, source := ResolveCost(&reportedCost, "claude-3-5-sonnet-20241022", 1000, 500, 0, 0, nil)

	if cost == nil || *cost != 0.05 {
		t.Errorf("expected cost 0.05, got %v", cost)
	}
	if source != "reported" {
		t.Errorf("expected source 'reported', got %s", source)
	}
}

func TestResolveCostComputed(t *testing.T) {
	// Create a price table
	prices := &ModelPrices{
		InputRate:         3.0,
		OutputRate:        15.0,
		CacheCreationRate: 3.75,
		CacheReadRate:     0.30,
	}
	priceTable := PriceTable{
		"claude-3-5-sonnet": prices,
	}

	cost, source := ResolveCost(nil, "claude-3-5-sonnet", 1000, 1000, 0, 0, priceTable)

	if cost == nil {
		t.Fatal("expected cost to be computed")
	}

	// 1000 input tokens at 3.0 per million = 0.003
	// 1000 output tokens at 15.0 per million = 0.015
	// Total = 0.018
	expected := 0.018
	if *cost != expected {
		t.Errorf("expected cost %f, got %f", expected, *cost)
	}

	if source != "computed" {
		t.Errorf("expected source 'computed', got %s", source)
	}
}

func TestResolveCostUnknownModel(t *testing.T) {
	// Empty price table
	priceTable := PriceTable{}

	cost, source := ResolveCost(nil, "unknown-model", 1000, 500, 0, 0, priceTable)

	if cost != nil {
		t.Errorf("expected nil cost for unknown model, got %v", cost)
	}
	if source != "" {
		t.Errorf("expected empty source, got %s", source)
	}
}

func TestResolveCostNoPriceTable(t *testing.T) {
	cost, source := ResolveCost(nil, "claude-3-5-sonnet", 1000, 500, 0, 0, nil)

	if cost != nil {
		t.Errorf("expected nil cost without price table, got %v", cost)
	}
	if source != "" {
		t.Errorf("expected empty source, got %s", source)
	}
}

func TestResolveCostWithCacheTokens(t *testing.T) {
	prices := &ModelPrices{
		InputRate:         3.0,
		OutputRate:        15.0,
		CacheCreationRate: 3.75,
		CacheReadRate:     0.30,
	}
	priceTable := PriceTable{
		"claude-3-5-sonnet": prices,
	}

	cost, source := ResolveCost(
		nil,
		"claude-3-5-sonnet",
		1000,  // input
		500,   // output
		1000,  // cache creation
		100,   // cache read
		priceTable,
	)

	if cost == nil {
		t.Fatal("expected cost to be computed")
	}

	// 1000 input at 3.0 = 0.003
	// 500 output at 15.0 = 0.0075
	// 1000 cache creation at 3.75 = 0.00375
	// 100 cache read at 0.30 = 0.00003
	// Total = 0.01428
	expected := 0.01428
	if *cost != expected {
		t.Errorf("expected cost ~%f, got %f", expected, *cost)
	}

	if source != "computed" {
		t.Errorf("expected source 'computed', got %s", source)
	}
}

func TestResolveCostReportedTakesPrecedence(t *testing.T) {
	reportedCost := 0.10
	prices := &ModelPrices{
		InputRate:  3.0,
		OutputRate: 15.0,
	}
	priceTable := PriceTable{
		"claude-3-5-sonnet": prices,
	}

	cost, source := ResolveCost(&reportedCost, "claude-3-5-sonnet", 1000, 500, 0, 0, priceTable)

	if cost == nil || *cost != 0.10 {
		t.Errorf("expected reported cost 0.10, got %v", cost)
	}
	if source != "reported" {
		t.Errorf("expected source 'reported', got %s", source)
	}
}
