// Demo file: intentional AIP violations for the review workflow to flag.
// DO NOT use this as a reference — it is non-compliant on purpose.
package users

import "context"

type User struct {
    ID   string
    Name string
}

type UserService struct{}

// AIP-127 violation: method name is snake_case and non-standard.
// AIP-131 violation: Get methods must return the singular resource.
func (s *UserService) fetch_user(ctx context.Context, id string) ([]*User, error) {
    return nil, nil
}

// AIP-132 violation: list methods must be named ListUsers and accept pagination.
func (s *UserService) RetrieveAllUsers(ctx context.Context) ([]*User, error) {
    return nil, nil
}

// AIP-135 violation: delete should be named DeleteUser and return Empty (or the soft-deleted resource only for soft-delete).
func (s *UserService) RemoveUser(ctx context.Context, id string) (*User, error) {
    return nil, nil
}
