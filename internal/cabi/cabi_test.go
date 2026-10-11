package cabi

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestDispatch(t *testing.T) {
	Serve(Plugin{Handle: func(method string, request []byte) (any, error) {
		switch method {
		case "echo":
			return json.RawMessage(request), nil
		case "missing":
			return nil, &Error{Code: "unknown_method", Message: "unknown method missing"}
		case "boom":
			return nil, errors.New("boom")
		}
		return nil, nil
	}})
	defer Serve(Plugin{})

	cases := []struct {
		method, want string
		wantErr      bool
	}{
		{"echo", `{"ok":true,"result":{"a":1}}`, false},
		{"missing", `{"ok":false,"error":{"code":"unknown_method","message":"unknown method missing"}}`, true},
		{"boom", `{"ok":false,"error":{"code":"plugin_error","message":"boom"}}`, true},
	}
	for _, tc := range cases {
		raw, err := Dispatch(tc.method, []byte(`{"a":1}`))
		if string(raw) != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s: got %s, err %v; want %s", tc.method, raw, err, tc.want)
		}
	}
}

func TestCallHostBeforeInit(t *testing.T) {
	if _, err := CallHost("host.log", nil); !errors.Is(err, ErrHostUnavailable) {
		t.Errorf("err = %v", err)
	}
}
