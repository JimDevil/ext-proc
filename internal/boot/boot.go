// Package boot wires up startup: config -> logger -> plugin chain.
// One Init function per step, with gerror chaining the failure cause.
package boot

import (
	"context"

	"github.com/gogf/gf/v2/errors/gerror"
)

// Init performs the full startup assembly. Any step failure exits the process
// with a clear cause.
func Init(ctx context.Context) error {
	if err := InitConfig(ctx); err != nil {
		return gerror.Wrap(err, "Init config error")
	}

	if err := InitLog(ctx); err != nil {
		return gerror.Wrap(err, "Init log error")
	}

	if err := InitChain(ctx); err != nil {
		return gerror.Wrap(err, "Init chain error")
	}

	return nil
}
