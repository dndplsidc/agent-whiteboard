package contextdigest_test

import (
	"testing"

	"github.com/edocsss/agent-whiteboard/internal/contextdigest"
	"github.com/stretchr/testify/require"
)

func TestCalculateKnownVector(t *testing.T) {
	require.Equal(t, "2955a3d16e16b3a1044e95e84aea4cd29b37440217bdaff323fb32e31b47159b", contextdigest.Calculate([]byte("# page"), []byte("creator summary")))
}

func TestCalculateBindsOrderLengthsMutationsAndEmptyValues(t *testing.T) {
	empty := contextdigest.Calculate(nil, nil)
	require.Equal(t, "f8c381b00017af05858533ce6ab4af46672abfd3844b765295bd11b926ebfe0e", empty)
	require.Equal(t, empty, contextdigest.Calculate([]byte{}, []byte{}))

	base := contextdigest.Calculate([]byte("a"), []byte("b"))
	require.Equal(t, "3cc7e6eb601fbd3b297e94336f32659c95df751449c2fc760b2fb1f0f2304d21", base)
	require.NotEqual(t, base, contextdigest.Calculate([]byte("b"), []byte("a")))
	require.NotEqual(t, base, contextdigest.Calculate([]byte("a!"), []byte("b")))
	require.NotEqual(t, base, contextdigest.Calculate([]byte("a"), []byte("b!")))
	require.NotEqual(t, contextdigest.Calculate([]byte("ab"), nil), contextdigest.Calculate([]byte("a"), []byte("b")))
}
