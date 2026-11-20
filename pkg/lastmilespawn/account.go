package lastmilespawn

import (
	"context"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go/middleware"
)

// AccountObserver records the account every successful sts:GetCallerIdentity
// made with a config carrying its Middleware reported. It makes no call
// itself. The zero value is ready to use.
type AccountObserver struct {
	mu       sync.Mutex
	account  string
	conflict bool
}

// observerID names the middleware on each client's stack.
const observerID = "spawn/lastmilespawn/observe-caller-identity"

// Middleware is an aws.Config APIOptions entry. It reads the response of a
// GetCallerIdentity, changes nothing, and adds itself only once to a stack.
func (o *AccountObserver) Middleware(stack *middleware.Stack) error {
	if _, ok := stack.Initialize.Get(observerID); ok {
		return nil
	}
	return stack.Initialize.Add(middleware.InitializeMiddlewareFunc(observerID,
		func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
			out, md, err := next.HandleInitialize(ctx, in)
			if err == nil {
				if id, ok := out.Result.(*sts.GetCallerIdentityOutput); ok && id != nil {
					o.record(aws.ToString(id.Account))
				}
			}
			return out, md, err
		}), middleware.After)
}

func (o *AccountObserver) record(account string) {
	if account == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case o.account == "":
		o.account = account
	case o.account != account:
		o.conflict = true
	}
}

// Account is the account the observed calls reported, or "" when none was
// observed or two named different accounts (a compute profile in another
// account, say: a caller could not tell whose footprint to read).
func (o *AccountObserver) Account() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conflict {
		return ""
	}
	return o.account
}

// Reset forgets what was observed.
func (o *AccountObserver) Reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.account, o.conflict = "", false
}
