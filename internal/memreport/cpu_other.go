//go:build !unix

package memreport

// processCPUSeconds is not available here; the report's CPU share is 0.
func processCPUSeconds() float64 { return 0 }
