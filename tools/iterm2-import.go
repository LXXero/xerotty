// Command iterm2-import converts .itermcolors (Apple plist XML) to xerotty TOML theme format.
//
// Usage: go run tools/iterm2-import.go Dracula.itermcolors > ~/.config/xerotty/themes/dracula.toml
package main

import (
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: iterm2-import <file.itermcolors>\n")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	colors, err := parsePlist(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse error: %v\n", err)
		os.Exit(1)
	}

	name := strings.TrimSuffix(os.Args[1], ".itermcolors")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}

	printTheme(name, colors)
}

type plistDict struct {
	Keys   []string
	Values map[string]interface{}
}

func parsePlist(data []byte) (map[string]string, error) {
	// Simple plist parser for iTerm2 color files
	type Plist struct {
		Dict struct {
			Items []string `xml:",any"`
		} `xml:"dict"`
	}

	// Use a more manual approach
	decoder := xml.NewDecoder(strings.NewReader(string(data)))
	colors := make(map[string]string)

	var currentKey string
	var inDict int
	var colorDict map[string]float64

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}

		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				inDict++
				if inDict == 2 {
					colorDict = make(map[string]float64)
				}
			case "key":
				var s string
				decoder.DecodeElement(&s, &t)
				currentKey = s
			case "real":
				var s string
				decoder.DecodeElement(&s, &t)
				if colorDict != nil {
					var f float64
					fmt.Sscanf(s, "%f", &f)
					colorDict[currentKey] = f
				}
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				if inDict == 2 && colorDict != nil {
					// Convert RGB floats to hex
					r := int(math.Round(colorDict["Red Component"] * 255))
					g := int(math.Round(colorDict["Green Component"] * 255))
					b := int(math.Round(colorDict["Blue Component"] * 255))
					hex := fmt.Sprintf("#%02X%02X%02X", r, g, b)

					// Map the parent key
					colors[currentKey] = hex
					colorDict = nil
				}
				inDict--
			}
		}
	}

	return colors, nil
}

func printTheme(name string, colors map[string]string) {
	fmt.Printf("[theme]\nname = %q\n\n", name)
	fmt.Println("[theme.colors]")

	if v, ok := colors["Foreground Color"]; ok {
		fmt.Printf("foreground = %q\n", v)
	}
	if v, ok := colors["Background Color"]; ok {
		fmt.Printf("background = %q\n", v)
	}
	if v, ok := colors["Cursor Color"]; ok {
		fmt.Printf("cursor = %q\n", v)
	}
	if v, ok := colors["Selected Text Color"]; ok {
		fmt.Printf("selection_fg = %q\n", v)
	}
	if v, ok := colors["Selection Color"]; ok {
		fmt.Printf("selection_bg = %q\n", v)
	}

	fmt.Println("\n[theme.colors.ansi]")
	ansiNames := []string{
		"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
		"bright_black", "bright_red", "bright_green", "bright_yellow",
		"bright_blue", "bright_magenta", "bright_cyan", "bright_white",
	}

	for i, name := range ansiNames {
		key := fmt.Sprintf("Ansi %d Color", i)
		if v, ok := colors[key]; ok {
			fmt.Printf("%-15s= %q\n", name, v)
		}
	}
}
