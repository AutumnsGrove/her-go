package layers

// Layer 4.5: Weather context.
// Current conditions so the bot can reference weather naturally.
// Only included if weather is configured and data is available.

func init() {
	Register(PromptLayer{
		Name:    "Weather",
		Order:   450,
		Stream:  StreamChat,
		Builder: buildChatWeather,
	})
}

func buildChatWeather(ctx *LayerContext) LayerResult {
	if ctx.WeatherClient == nil {
		return LayerResult{}
	}
	summary := ctx.WeatherClient.FormatContext()
	if summary == "" {
		return LayerResult{}
	}
	return LayerResult{Content: "# Current Weather\n" + summary}
}
