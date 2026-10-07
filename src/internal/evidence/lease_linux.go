//go:build linux

package evidence

import (
	"fmt"
	"os"
)

// AcquireHostLease takes the one fixed host lock at the production root. It
// requires root and a shared mount namespace, matching the T4 lock opener. The
// namespace check is retained on the lease and re-run before every effect so a
// later mount replacement cannot redirect the fixed root.
func AcquireHostLease() (*Lease, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("%w: root required", ErrEvidenceTrust)
	}
	if err := productionNamespaceCheck(); err != nil {
		return nil, err
	}
	l, err := acquireHostLeaseAtTrust(ProductionRoot, productionTrust)
	if err != nil {
		return nil, err
	}
	l.namespace = productionNamespaceCheck
	return l, nil
}

// productionNamespaceCheck requires the caller and PID1 to share one mount
// namespace so a mount replacement cannot redirect the fixed root.
func productionNamespaceCheck() error {
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return err
	}
	init, err := os.Readlink("/proc/1/ns/mnt")
	if err != nil {
		return err
	}
	if self == "" || self != init {
		return fmt.Errorf("%w: split mount namespace", ErrEvidenceTrust)
	}
	return nil
}

// productionSupervisorBinding is deliberately unavailable. Spec 6.2/6.7 fix the
// fields a runtime-producer qualification must match (the actual supervisor
// executable, effective service definition and provisioning revision) but do
// not define the protected provenance or the live kernel/service identity
// verification for the evidence leaf. The previous implementation hashed a
// guessed fixed unit-file path and parsed an invented revision comment, which
// cannot prove the running process is the qualified service and would let an
// unqualified executable placed at that path qualify. Manufacturing such a
// binding is prohibited, so production fails closed pending the missing
// contract; offline tests inject the binding through the package-private lease
// seam.
func productionSupervisorBinding() (supervisorBinding, error) {
	return supervisorBinding{}, fmt.Errorf("%w: supervisor runtime binding contract undefined", ErrUnsupportedEvidenceHost)
}
