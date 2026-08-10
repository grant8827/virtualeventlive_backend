package services

import "crypto/rand"

// Unambiguous when read off a screen or printed ticket stub — no 0/O or
// 1/I/L confusion.
const ticketCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
const ticketCodeLength = 10

// GenerateTicketCode returns a short, human-typable ticket code: the viewing
// code printed on the ticket and used as its access_token / URL param. At
// 32^10 possibilities, collisions are astronomically unlikely; the DB's
// UNIQUE constraint on access_token is the backstop.
func GenerateTicketCode() string {
	b := make([]byte, ticketCodeLength)
	rand.Read(b)
	out := make([]byte, ticketCodeLength)
	for i, v := range b {
		out[i] = ticketCodeAlphabet[int(v)%len(ticketCodeAlphabet)]
	}
	return string(out)
}
