package ingestor

type Application struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
}

type Config struct {
	Applications []Application `json:"applications"`
}
