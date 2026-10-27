package main

import (
	"errors"
	"fmt"
	"os"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// wgkey writes a new WireGuard private key to a file that only the owner can read, and prints the public key.
// The private key is never printed.
func wgkey(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: tcpbench wgkey FILE")
	}
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(args[0], []byte(k.String()), 0o600); err != nil {
		return err
	}
	fmt.Println(k.PublicKey().String())
	return nil
}
