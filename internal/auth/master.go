package auth

import "github.com/DiegohNY/costlane/internal/obs"

// VerifyMaster reports whether a presented credential is the configured
// master key.
//
// The comparison runs in constant time over fixed-length hashes, so neither
// the value nor the length of the configured key leaks through timing. An
// unset master key verifies nothing: a deployment that forgot to configure
// one must not treat an empty Authorization header as administrative.
func VerifyMaster(configured, presented obs.Secret) bool {
	if !configured.IsSet() || !presented.IsSet() {
		return false
	}
	return configured.Equal(presented)
}
