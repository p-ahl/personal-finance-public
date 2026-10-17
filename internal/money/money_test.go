package money

import "testing"

func TestParseMinor(t *testing.T) {
	cases := []struct {
		in    string
		scale int
		want  int64
	}{
		{"-1234.5600", 2, -123456},
		{"0.5100", 2, 51},
		{"12345.6700", 2, 1234567},
		{"0", 2, 0},
		{"+5", 2, 500},
		{".5", 2, 50},
		{"123.4560000000", 3, 123456},
		{"-0.0100", 2, -1},
	}
	for _, c := range cases {
		got, err := ParseMinor(c.in, c.scale)
		if err != nil || got != c.want {
			t.Errorf("ParseMinor(%q,%d) = %d, %v; want %d", c.in, c.scale, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1e5", "12.345"} {
		if _, err := ParseMinor(bad, 2); err == nil {
			t.Errorf("ParseMinor(%q) expected error", bad)
		}
	}
}

func TestParseMinorRound(t *testing.T) {
	for in, want := range map[string]int64{"1234.5683": 123457, "1234.5649": 123456, "-1.005": -101, "1.004": 100} {
		got, err := ParseMinorRound(in, 2)
		if err != nil || got != want {
			t.Errorf("ParseMinorRound(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

// Decimal precision: summing 0.10 ten thousand times is exact in minor units (a float would drift).
func TestNoFloatDrift(t *testing.T) {
	var sum int64
	for i := 0; i < 10000; i++ {
		v, _ := ParseMinor("0.10", 2)
		sum += v
	}
	if sum != 100000 || Format(sum, 2) != "1000.00" {
		t.Fatalf("sum = %d (%s)", sum, Format(sum, 2))
	}
	var f float64
	for i := 0; i < 10000; i++ {
		f += 0.1
	}
	if f == 1000.0 {
		t.Log("float happened to be exact")
	}
}

func TestFormat(t *testing.T) {
	if got := Format(-5, 2); got != "-0.05" {
		t.Errorf("got %s", got)
	}
	if got := FormatUSD(-1234500); got != "-12,345.00" {
		t.Errorf("got %s", got)
	}
	if got := FormatUSD(12345678901); got != "123,456,789.01" {
		t.Errorf("got %s", got)
	}
}
