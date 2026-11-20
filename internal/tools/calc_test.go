package tools

import "testing"

func TestEval(t *testing.T) {
	cases := map[string]float64{
		"1+2*3":            7,
		"(1+2)*3":          9,
		"2^3^2":            512,
		"-4+10":            6,
		"1.5e3/3":          500,
		"1e-3*1000":        1,
		"sqrt(16)+abs(-2)": 6,
		"10%4":             2,
	}
	for expr, want := range cases {
		got, err := Eval(expr)
		if err != nil || got != want {
			t.Errorf("Eval(%q) = %v, %v; want %v", expr, got, err, want)
		}
	}
	if _, err := Eval("2+"); err == nil {
		t.Error("expected error for incomplete expression")
	}
}
