package main

import (
	"context"
	"fmt"
)

func main() {
	ctx := context.Background()

	keyA := "keyA"
	ctxA := context.WithValue(ctx, keyA, "value from ctxA")

	keyC := keyA // same key with keyA
	ctxC := context.WithValue(ctx, keyC, "value from ctxC")

	fmt.Println(ctxC.Value(keyC)) // node from ctxC
	fmt.Println(ctxA.Value(keyA)) // node from ctxA
	fmt.Println("==============")

	keyB := "keyB"
	ctxB := context.WithValue(ctxA, keyB, "value from ctxB") // child ctx of ctxA

	keyD := "keyD"
	ctxD := context.WithValue(ctxC, keyD, "value from ctxD") // child ctx of ctxC

	fmt.Println(ctxB.Value(keyA)) // find node from ctxA
	fmt.Println(ctxD.Value(keyA)) // find node from ctxC
}
