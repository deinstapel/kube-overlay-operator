package iputil

import (
	"errors"
	"net"
)

// ErrAddressPoolExhausted is returned if no IP could be allocated
var ErrAddressPoolExhausted = errors.New("address pool exhausted")

// FirstFreeHost returns the first unallocated IP address from the given cidr
func FirstFreeHost(cidr string, allocated map[string]bool) (string, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", err
	}
	// Increment to skip "network address"...
	// Leftovers from ancient times
	inc(ip)

	for ; ipnet.Contains(ip); inc(ip) {
		if allocated[ip.String()] {
			continue
		}
		return ip.String(), nil
	}
	return "", ErrAddressPoolExhausted
}

// http://play.golang.org/p/m8TNTtygK0
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}
