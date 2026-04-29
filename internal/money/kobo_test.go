package money

import (
	"errors"
	"testing"
)

func TestParseKobo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    Kobo
		wantErr error
	}{
		{in: "1", want: 1},
		{in: "10", want: 10},
		{in: "10000", want: 10000},
		{in: "100000000", want: 100000000},
		{in: "9223372036854775807", want: 9223372036854775807},

		{in: "", wantErr: ErrEmpty},

		{in: "0", wantErr: ErrNonPositive},
		{in: "-1", wantErr: ErrNotInteger},
		{in: "+1", wantErr: ErrNotInteger},

		{in: "01", wantErr: ErrNotInteger},
		{in: "007", wantErr: ErrNotInteger},

		{in: "10.00", wantErr: ErrNotInteger},
		{in: "10.", wantErr: ErrNotInteger},
		{in: ".10", wantErr: ErrNotInteger},
		{in: "1e4", wantErr: ErrNotInteger},
		{in: "1E4", wantErr: ErrNotInteger},
		{in: "1_000", wantErr: ErrNotInteger},
		{in: "1,000", wantErr: ErrNotInteger},
		{in: " 10", wantErr: ErrNotInteger},
		{in: "10 ", wantErr: ErrNotInteger},
		{in: "\t10", wantErr: ErrNotInteger},
		{in: "10\n", wantErr: ErrNotInteger},
		{in: "0x10", wantErr: ErrNotInteger},
		{in: "abc", wantErr: ErrNotInteger},
		{in: "10abc", wantErr: ErrNotInteger},

		{in: "9223372036854775808", wantErr: ErrOutOfRange},
		{in: "99999999999999999999", wantErr: ErrOutOfRange},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseKobo(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseKobo(%q): err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseKobo(%q): unexpected err %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseKobo(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseNaira(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    Kobo
		wantErr error
	}{
		// happy paths
		{in: "1", want: 100},
		{in: "100", want: 10_000},
		{in: "10000", want: 1_000_000},
		{in: "10000.5", want: 1_000_050},
		{in: "10000.50", want: 1_000_050},
		{in: "10000.55", want: 1_000_055},
		{in: "1.99", want: 199},
		{in: "0.50", want: 50},
		{in: "0.99", want: 99},

		// rejected
		{in: "", wantErr: ErrEmpty},
		{in: "0", wantErr: ErrNonPositive},
		{in: "0.00", wantErr: ErrNonPositive},
		{in: "-1", wantErr: ErrNotInteger},
		{in: "+1", wantErr: ErrNotInteger},
		{in: "10.555", wantErr: ErrNotInteger},
		{in: "10.", wantErr: ErrNotInteger},
		{in: ".50", wantErr: ErrNotInteger},
		{in: "10..50", wantErr: ErrNotInteger},
		{in: "01", wantErr: ErrNotInteger},
		{in: "010.50", wantErr: ErrNotInteger},
		{in: "1e4", wantErr: ErrNotInteger},
		{in: "1,000", wantErr: ErrNotInteger},
		{in: " 10", wantErr: ErrNotInteger},
		{in: "10 ", wantErr: ErrNotInteger},
		{in: "abc", wantErr: ErrNotInteger},

		// overflow: 92_233_720_368_547_758 naira would be int64 max kobo;
		// anything beyond is rejected.
		{in: "92233720368547759", wantErr: ErrOutOfRange},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseNaira(tc.in)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseNaira(%q): err = %v, want %v", tc.in, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNaira(%q): unexpected err %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseNaira(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestKoboFormatting(t *testing.T) {
	t.Parallel()
	if got := Kobo(10000).String(); got != "10000" {
		t.Fatalf("String() = %q, want %q", got, "10000")
	}
	if got := Kobo(0).String(); got != "0" {
		t.Fatalf("String() = %q, want %q", got, "0")
	}
	if got := Kobo(10000).Int64(); got != 10000 {
		t.Fatalf("Int64() = %d, want 10000", got)
	}
}

func TestKoboNaira(t *testing.T) {
	t.Parallel()
	cases := []struct {
		k    int64
		want string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{50, "0.50"},
		{99, "0.99"},
		{100, "1.00"},
		{1050, "10.50"},
		{1000000, "10000.00"},
		{99000000, "990000.00"},
		{-500, "-5.00"},
		{-1, "-0.01"},
	}
	for _, c := range cases {
		if got := Kobo(c.k).Naira(); got != c.want {
			t.Fatalf("Kobo(%d).Naira() = %q, want %q", c.k, got, c.want)
		}
	}
}
