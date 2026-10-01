package validate

import (
	"strings"
	"testing"
)

func TestValidateAPIKeyName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid name", input: "dev key", wantErr: false},
		{name: "single char", input: "a", wantErr: false},
		{name: "unicode name", input: "ключ", wantErr: false},
		{name: "empty", input: "", wantErr: true},
		{name: "max length ok", input: strings.Repeat("a", 128), wantErr: false},
		{name: "too long", input: strings.Repeat("a", 129), wantErr: true},
		{name: "invalid utf-8", input: "\xff\xfe", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAPIKeyName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateAPIKeyName(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}
