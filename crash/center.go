package main

// center returns the top-left corner that centres a w×h box in the screen.
func center(screenW, screenH, w, h int) (int, int) {
	return max((screenW-w)/2, 0), max((screenH-h)/2, 0)
}
