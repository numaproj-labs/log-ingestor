package ingestor

type Application struct {
	Namespace  string   `json:"namespace"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Containers []string `json:"containers"`
}

type Config struct {
	Applications []Application `json:"applications"`
}
