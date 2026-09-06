package commands

import "context"

const adminDeniedMessage = "You are not allowed to use admin commands."

// RequireAdmin requires the command actor to be an admin canonical user.
func RequireAdmin(auth PrincipalAuthorizer) Middleware {
	return func(next Handler) Handler {
		return HandlerFunc{
			DefinitionValue: next.Definition(),
			ExecuteFunc: func(ctx context.Context, req Request) (Result, error) {
				if !req.Principal.Authenticated() || auth == nil {
					return Result{Text: adminDeniedMessage}, nil
				}
				isAdmin, err := IsPrincipalAdmin(auth, req.Principal)
				if err != nil {
					return Result{}, err
				}
				if !isAdmin {
					return Result{Text: adminDeniedMessage}, nil
				}
				return next.Execute(ctx, req)
			},
		}
	}
}
