// Copyright (c) 2019 IoTeX Foundation
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package protocol

import (
	"context"
	"sync"

	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/action"
)

type (
	// Nonce defines a function to return the nonce of a given address
	Nonce func(string) (uint64, error)
	// GenericValidator is the validator for generic action verification
	GenericValidator struct {
		mu    sync.RWMutex
		nonce Nonce
	}
)

// NewGenericValidator constructs a new genericValidator
func NewGenericValidator(nonce Nonce) *GenericValidator {
	return &GenericValidator{
		nonce: nonce,
	}
}

// Validate validates a generic action
func (v *GenericValidator) Validate(ctx context.Context, act action.SealedEnvelope) error {
	actionCtx := MustGetActionCtx(ctx)
	// Reject action with insufficient gas limit
	intrinsicGas, err := act.IntrinsicGas()
	if intrinsicGas > act.GasLimit() || err != nil {
		return errors.Wrap(action.ErrInsufficientBalanceForGas, "insufficient gas")
	}
	// Verify action using action sender's public key
	if err := action.Verify(act); err != nil {
		return errors.Wrap(err, "failed to verify action signature")
	}
	// Reject action if nonce is too low
	confirmedNonce, err := v.nonce(actionCtx.Caller.String())
	if err != nil {
		return errors.Wrapf(err, "invalid nonce value of account %s", actionCtx.Caller.String())
	}

	pendingNonce := confirmedNonce + 1
	if act.Nonce() > 0 && pendingNonce > act.Nonce() {
		return errors.Wrap(action.ErrNonce, "nonce is too low")
	}
	return nil
}
