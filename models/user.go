package models

import "time"

type User struct {
	ID               string    `json:"id"`
	Email            string    `json:"email"`
	PasswordHash     string    `json:"-"`
	Role             string    `json:"role"`
	FullName         string    `json:"full_name"`
	Phone            string    `json:"phone"`
	AddressLine1     string    `json:"address_line1"`
	AddressLine2     string    `json:"address_line2,omitempty"`
	City             string    `json:"city"`
	State            string    `json:"state"`
	PostalCode       string    `json:"postal_code"`
	Country          string    `json:"country"`
	OrganizationName string    `json:"organization_name,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}
