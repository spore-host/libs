package i18n

// Accessibility helpers for WCAG 2 compliance
// Provides emoji and symbol functions that respect accessibility settings

// Common emoji mappings
var emojiMap = map[string]string{
	"rocket":                  "🚀",
	"wizard":                  "🧙",
	"package":                 "📦",
	"globe":                   "🌍",
	"money":                   "💰",
	"money_bag":               "💰",
	"dollar":                  "💵",
	"clock":                   "⏱️",
	"key":                     "🔑",
	"label":                   "🏷️",
	"tag":                     "🏷️",
	"check":                   "✅",
	"cross":                   "❌",
	"warning":                 "⚠️",
	"hourglass":               "⏳",
	"plug":                    "🔌",
	"mushroom":                "🍄",
	"search":                  "🔍",
	"magnifying_glass":        "🔍",
	"magnifying_glass_tilted": "🔎",
	"chart":                   "📊",
	"location":                "📍",
	"pushpin":                 "📍",
	"laptop":                  "💻",
	"computer":                "💻",
	"gear":                    "⚙️",
	"sparkles":                "✨",
	"party":                   "🎉",
	"alert":                   "🚨",
	"megaphone":               "📢",
	"sleep":                   "💤",
	"zzz":                     "💤",
	"stop":                    "🔴",
	"memo":                    "📝",
	"books":                   "📚",
	"clipboard":               "📋",
	"lightbulb":               "💡",
	"wrench":                  "🔧",
	"gpu":                     "🎮",
	"video_game":              "🎮",
	"flag_us":                 "🇺🇸",
	"flag_eu":                 "🇪🇺",
	"flag_asia":               "🌏",
	"one":                     "1️⃣",
	"two":                     "2️⃣",
	"three":                   "3️⃣",
	"four":                    "4️⃣",
}

// ASCII alternatives for accessibility mode
var symbolMap = map[string]string{
	"success":  "✅",  // Visual
	"error":    "❌",  // Visual
	"warning":  "⚠️", // Visual
	"info":     "ℹ️", // Visual
	"pending":  "⏳",  // Visual
	"progress": "⏳",  // Visual
	"complete": "✅",  // Visual
	"failed":   "❌",  // Visual
	"skip":     "⏭️", // Visual
	"pause":    "⏸️", // Visual
}

// ASCII alternatives for accessibility mode
var accessibleSymbolMap = map[string]string{
	"success":  "[✓]",
	"error":    "[✗]",
	"warning":  "[!]",
	"info":     "[i]",
	"pending":  "[*]",
	"progress": "[*]",
	"complete": "[✓]",
	"failed":   "[✗]",
	"skip":     "[-]",
	"pause":    "[=]",
}

// Emoji returns an emoji character, or empty string if emoji disabled
func (l *Localizer) Emoji(name string) string {
	if l.noEmoji || l.accessibilityMode {
		return ""
	}

	if emoji, ok := emojiMap[name]; ok {
		return emoji
	}

	return ""
}

// Symbol returns a status symbol (emoji or ASCII alternative)
func (l *Localizer) Symbol(name string) string {
	if l.accessibilityMode {
		// Return ASCII alternative
		if symbol, ok := accessibleSymbolMap[name]; ok {
			return symbol
		}
		return "[?]"
	}

	if l.noEmoji {
		// Return ASCII alternative (simpler than emoji)
		if symbol, ok := accessibleSymbolMap[name]; ok {
			return symbol
		}
		return "[?]"
	}

	// Return visual symbol (may include emoji)
	if symbol, ok := symbolMap[name]; ok {
		return symbol
	}

	return "?"
}

// FormatStatus formats a status message with appropriate symbol
func (l *Localizer) FormatStatus(status, message string) string {
	symbol := l.Symbol(status)

	if l.accessibilityMode {
		// More explicit format for screen readers
		return symbol + " " + message
	}

	// Visual format
	return symbol + " " + message
}

// Global convenience functions

// Emoji returns an emoji using the global localizer
func Emoji(name string) string {
	if Global == nil {
		return ""
	}
	return Global.Emoji(name)
}

// Symbol returns a status symbol using the global localizer
func Symbol(name string) string {
	if Global == nil {
		return "?"
	}
	return Global.Symbol(name)
}

// FormatStatus formats a status message using the global localizer
func FormatStatus(status, message string) string {
	if Global == nil {
		return message
	}
	return Global.FormatStatus(status, message)
}
