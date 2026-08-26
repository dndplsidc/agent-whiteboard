package common_test

import (
	"math"
	"testing"
	"time"

	"github.com/dndplsidc/agent-whiteboard/internal/common"
	"github.com/stretchr/testify/require"
)

func TestResolveExpiration(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	zero := int64(0)
	oneHour := int64(3600)
	negative := int64(-1)
	overflow := int64(math.MaxInt64)
	current := now.Add(30 * time.Minute)

	tests := []struct {
		name        string
		resolve     func() (*time.Time, error)
		want        *time.Time
		wantErr     string
		wantErrCode common.ErrorCode
	}{
		{
			name:    "create zero is permanent",
			resolve: func() (*time.Time, error) { return common.ResolveCreateExpiration(now, 86400, &zero) },
		},
		{
			name:    "create supplied duration",
			resolve: func() (*time.Time, error) { return common.ResolveCreateExpiration(now, 86400, &oneHour) },
			want:    timePtr(now.Add(time.Hour)),
		},
		{
			name:    "create default duration",
			resolve: func() (*time.Time, error) { return common.ResolveCreateExpiration(now, oneHour, nil) },
			want:    timePtr(now.Add(time.Hour)),
		},
		{
			name:    "update omitted preserves current",
			resolve: func() (*time.Time, error) { return common.ResolveUpdateExpiration(now, &current, nil) },
			want:    &current,
		},
		{
			name:    "update zero becomes permanent",
			resolve: func() (*time.Time, error) { return common.ResolveUpdateExpiration(now, &current, &zero) },
		},
		{
			name:        "negative expiration is rejected",
			resolve:     func() (*time.Time, error) { return common.ResolveCreateExpiration(now, oneHour, &negative) },
			wantErr:     "expiration must not be negative",
			wantErrCode: common.CodeInvalidRequest,
		},
		{
			name:        "positive unix overflow is rejected",
			resolve:     func() (*time.Time, error) { return common.ResolveCreateExpiration(now, oneHour, &overflow) },
			wantErr:     "expiration overflows unix time",
			wantErrCode: common.CodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.resolve()
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				require.True(t, common.HasCode(err, tt.wantErrCode))
				require.Nil(t, got)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestIsExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	tests := []struct {
		name      string
		expiresAt *time.Time
		want      bool
	}{
		{name: "permanent", expiresAt: nil, want: false},
		{name: "before expiration", expiresAt: timePtr(now.Add(time.Second)), want: false},
		{name: "at expiration", expiresAt: &now, want: true},
		{name: "after expiration", expiresAt: timePtr(now.Add(-time.Second)), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, common.IsExpired(now, tt.expiresAt))
		})
	}
}

func timePtr(value time.Time) *time.Time {
	return &value
}
