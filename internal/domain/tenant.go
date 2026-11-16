package domain

import "time"

type TenantStatus string

const (
	TenantStatusActive TenantStatus = "active"
)

type Tenant struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Status    TenantStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
}
