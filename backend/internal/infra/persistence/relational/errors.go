package relational

import (
	"errors"

	"github.com/owen891/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return repository.ErrNotFound
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return repository.ErrConflict
	}
	return err
}
