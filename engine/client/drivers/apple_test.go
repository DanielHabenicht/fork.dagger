package drivers

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppleRunArgsPrivilegedGrantsAllCapabilities(t *testing.T) {
	// Apple's `container` has no `--privileged` flag; the equivalent is
	// `--cap-add ALL`. Since `container` 1.0.0 the default capability set no
	// longer includes CAP_SYS_ADMIN, which the engine needs to bind-mount
	// /etc/resolv.conf at startup, so this must be passed when privileged.
	args, _, err := apple{}.runArgs("dagger-engine-test", runOpts{
		image:      "registry.dagger.io/engine:test",
		privileged: true,
	})
	require.NoError(t, err)
	idx := slices.Index(args, "--cap-add")
	require.NotEqual(t, -1, idx, "expected --cap-add in args: %v", args)
	require.Less(t, idx+1, len(args))
	require.Equal(t, "ALL", args[idx+1])
}

func TestAppleRunArgsNotPrivilegedOmitsCapabilities(t *testing.T) {
	args, _, err := apple{}.runArgs("dagger-engine-test", runOpts{
		image:      "registry.dagger.io/engine:test",
		privileged: false,
	})
	require.NoError(t, err)
	require.NotContains(t, args, "--cap-add")
}

func TestAppleRunArgsPrivilegedEnablesIPForwarding(t *testing.T) {
	// Each apple `container` runs in its own Linux VM where
	// /proc/sys/net/ipv4/ip_forward is read-only, so the engine's CNI bridge
	// setup fails unless IP forwarding is enabled via a kernel boot arg.
	args, _, err := apple{}.runArgs("dagger-engine-test", runOpts{
		image:      "registry.dagger.io/engine:test",
		privileged: true,
	})
	require.NoError(t, err)
	idx := slices.Index(args, "--kernel-arg")
	require.NotEqual(t, -1, idx, "expected --kernel-arg in args: %v", args)
	require.Less(t, idx+1, len(args))
	require.Equal(t, "sysctl.net.ipv4.ip_forward=1", args[idx+1])
}

func TestAppleRunArgsNotPrivilegedOmitsKernelArg(t *testing.T) {
	args, _, err := apple{}.runArgs("dagger-engine-test", runOpts{
		image:      "registry.dagger.io/engine:test",
		privileged: false,
	})
	require.NoError(t, err)
	require.NotContains(t, args, "--kernel-arg")
}
