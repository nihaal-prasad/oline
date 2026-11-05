package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nihaal-prasad/oline/internal/sandbox"
)

// destroyItem is one destroyable, oline-managed environment.
type destroyItem struct {
	id    string // the name passed to the backend's Destroy call
	kind  string // "wsl" | "qemu-image" | "qemu-engagement"
	label string // human line shown in the menu
}

// runDestroy lists ONLY oline-managed environments (WSL distros + QEMU images and
// persisted engagement disks whose name carries the oline- prefix) and destroys
// the one the user selects, after they retype its exact name. It never lists or
// touches anything without the oline- prefix, so a WSL distro or QEMU image the
// user keeps for other purposes can never be affected. It does nothing unless the
// user explicitly runs `oline -destroy` and confirms.
func runDestroy() {
	ctx := context.Background()
	var items []destroyItem

	if wsls, err := sandbox.NewWSL().Scan(ctx); err == nil {
		for _, c := range wsls {
			if c.Managed {
				items = append(items, destroyItem{id: c.ID, kind: "wsl", label: c.ID + "   (WSL distro)"})
			}
		}
	}
	if qs, err := sandbox.NewQEMU().Scan(ctx); err == nil {
		for _, c := range qs {
			if c.Managed {
				items = append(items, destroyItem{id: c.ID, kind: "qemu-image", label: c.ID + "   (QEMU base image)"})
			}
		}
	}
	if qc, ok := sandbox.NewQEMU().(sandbox.QEMUControl); ok {
		for _, e := range qc.ResumableEngagements() {
			items = append(items, destroyItem{id: e, kind: "qemu-engagement", label: e + "   (persisted QEMU engagement disk)"})
		}
	}

	if len(items) == 0 {
		fmt.Println("oline: no oline-managed environments found — nothing oline created, so nothing to destroy.")
		return
	}

	fmt.Println("oline-managed environments — only these can be destroyed.")
	fmt.Println("(Anything you use for other purposes is never listed and never touched.)")
	fmt.Println()
	for i, it := range items {
		fmt.Printf("  %d) %s\n", i+1, it.label)
	}
	fmt.Print("\nEnter the number to destroy (blank to cancel): ")

	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() {
		fmt.Println("cancelled.")
		return
	}
	sel := strings.TrimSpace(in.Text())
	if sel == "" {
		fmt.Println("cancelled.")
		return
	}
	n, err := strconv.Atoi(sel)
	if err != nil || n < 1 || n > len(items) {
		fmt.Fprintln(os.Stderr, "oline: invalid selection — nothing destroyed.")
		os.Exit(2)
	}
	it := items[n-1]

	fmt.Printf("\nThis permanently destroys %q (%s). This cannot be undone.\n", it.id, it.kind)
	fmt.Printf("Type the name %q to confirm: ", it.id)
	if !in.Scan() {
		fmt.Println("cancelled.")
		return
	}
	if strings.TrimSpace(in.Text()) != it.id {
		fmt.Println("name did not match — cancelled. Nothing was destroyed.")
		return
	}

	var derr error
	switch it.kind {
	case "wsl":
		derr = sandbox.NewWSL().Destroy(ctx, it.id)
	case "qemu-image":
		derr = sandbox.NewQEMU().Destroy(ctx, it.id)
	case "qemu-engagement":
		if qc, ok := sandbox.NewQEMU().(sandbox.QEMUControl); ok {
			derr = qc.DestroyEngagement(ctx, it.id)
		} else {
			derr = fmt.Errorf("qemu backend unavailable")
		}
	}
	if derr != nil {
		fmt.Fprintln(os.Stderr, "oline: destroy:", derr)
		os.Exit(1)
	}
	fmt.Printf("destroyed %q.\n", it.id)
}
