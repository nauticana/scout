package domain

import (
	"errors"
	"math"
	"testing"
)

func TestSearchGroundingValidate(t *testing.T) {
	latitude, longitude, far, nan := 30.2672, -97.7431, 181.0, math.NaN()
	cases := map[string]struct {
		search *SearchGrounding
		valid  bool
	}{
		"absent":              {nil, true},
		"unbounded":           {&SearchGrounding{}, true},
		"negative bound":      {&SearchGrounding{MaxSearches: -1}, false},
		"named place":         {&SearchGrounding{Location: &SearchLocation{City: "Austin", Region: "Texas", Country: "US", Timezone: "America/Chicago"}}, true},
		"coordinates only":    {&SearchGrounding{Location: &SearchLocation{Latitude: &latitude, Longitude: &longitude}}, true},
		"empty location":      {&SearchGrounding{Location: &SearchLocation{}}, false},
		"lowercase country":   {&SearchGrounding{Location: &SearchLocation{Country: "us"}}, false},
		"alpha-3 country":     {&SearchGrounding{Location: &SearchLocation{Country: "USA"}}, false},
		"padded city":         {&SearchGrounding{Location: &SearchLocation{City: " Austin"}}, false},
		"invalid timezone":    {&SearchGrounding{Location: &SearchLocation{Timezone: "America/Nowhere"}}, false},
		"unpaired latitude":   {&SearchGrounding{Location: &SearchLocation{Latitude: &latitude}}, false},
		"longitude past 180":  {&SearchGrounding{Location: &SearchLocation{Latitude: &latitude, Longitude: &far}}, false},
		"latitude not finite": {&SearchGrounding{Location: &SearchLocation{Latitude: &nan, Longitude: &longitude}}, false},
	}
	for name, test := range cases {
		err := test.search.Validate()
		if test.valid != (err == nil) || !test.valid && !errors.Is(err, ErrValidation) {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
}

func TestNarrowedSearchTakesTheTaskLocation(t *testing.T) {
	step := ToolLoopConfig{Search: &SearchGrounding{MaxSearches: 4, Location: &SearchLocation{Country: "US"}}}
	inherited, err := step.NarrowedSearch(&SearchGrounding{})
	if err != nil || inherited.Location.Country != "US" || inherited.MaxSearches != 4 {
		t.Fatalf("inherited = %+v, %v", inherited, err)
	}
	task := &SearchLocation{City: "Austin", Country: "US"}
	narrowed, err := step.NarrowedSearch(&SearchGrounding{MaxSearches: 9, Location: task})
	if err != nil || narrowed.Location != task || narrowed.MaxSearches != 4 {
		t.Fatalf("narrowed = %+v, %v", narrowed, err)
	}
}

func TestNarrowedSearchRejectsInvalidConfiguration(t *testing.T) {
	valid := &SearchGrounding{}
	invalid := &SearchGrounding{MaxSearches: -1}
	if _, err := (ToolLoopConfig{Search: invalid}).NarrowedSearch(valid); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid step: %v", err)
	}
	if _, err := (ToolLoopConfig{Search: valid}).NarrowedSearch(invalid); !errors.Is(err, ErrValidation) {
		t.Fatalf("invalid task: %v", err)
	}
}
