package utils

import (
	"fmt"
	"runtime"
	"time"
)

func PrintStats() {
	ticker := time.NewTicker(time.Second * 2)
	defer ticker.Stop()

	for range ticker.C {
		printStats()
	}

}
func printStats() {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	fmt.Printf("Go Runtime Stats\n")
	fmt.Printf("================\n")

	// Memory statistics
	fmt.Printf("Alloc: %v MiB\n", bToMb(mem.Alloc))           // Memory currently allocated
	fmt.Printf("TotalAlloc: %v MiB\n", bToMb(mem.TotalAlloc)) // Total memory allocated (includes freed memory)
	fmt.Printf("Sys: %v MiB\n", bToMb(mem.Sys))               // Memory obtained from the system
	fmt.Printf("Lookups: %v\n", mem.Lookups)                  // Pointer lookups
	fmt.Printf("Mallocs: %v\n", mem.Mallocs)                  // Number of mallocs
	fmt.Printf("Frees: %v\n", mem.Frees)                      // Number of frees

	// Garbage collection statistics
	fmt.Printf("HeapAlloc: %v MiB\n", bToMb(mem.HeapAlloc))       // Heap memory currently allocated
	fmt.Printf("HeapSys: %v MiB\n", bToMb(mem.HeapSys))           // Heap memory obtained from the system
	fmt.Printf("HeapIdle: %v MiB\n", bToMb(mem.HeapIdle))         // Heap memory waiting to be used
	fmt.Printf("HeapInuse: %v MiB\n", bToMb(mem.HeapInuse))       // Heap memory currently in use
	fmt.Printf("HeapReleased: %v MiB\n", bToMb(mem.HeapReleased)) // Heap memory released to OS
	fmt.Printf("HeapObjects: %v\n", mem.HeapObjects)              // Number of objects on the heap

	// Stack statistics
	fmt.Printf("StackInuse: %v MiB\n", bToMb(mem.StackInuse)) // Stack memory in use
	fmt.Printf("StackSys: %v MiB\n", bToMb(mem.StackSys))     // Stack memory obtained from the system

	// General stats
	fmt.Printf("NumGC: %v\n", mem.NumGC)                 // Number of garbage collections
	fmt.Printf("GCCPUFraction: %v\n", mem.GCCPUFraction) // Fraction of CPU used for GC

	// CPU statistics
	fmt.Printf("NumGoroutine: %v\n", runtime.NumGoroutine()) // Number of goroutines
	fmt.Printf("NumCPU: %v\n", runtime.NumCPU())             // Number of available CPUs
	fmt.Printf("NumCgoCall: %v\n", runtime.NumCgoCall())     // Number of Cgo calls
}

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}
