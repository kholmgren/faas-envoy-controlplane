package internal

type Manifest struct {
	Name     string `yaml:"name"`
	Location string `yaml:"location"`

	Environment map[string]string `yaml:"environment"`

	Authorization struct {
		Extensions map[string]string `yaml:"extensions"`
	} `yaml:"authorization"`

	Paths map[string]struct {
		Handler       string `yaml:"handler"`
		Authorization struct {
			ObjectIdPtr string            `yaml:"objectid_ptr"`
			Extensions  map[string]string `yaml:"extensions"`
		} `yaml:"authorization"`
	} `yaml:"paths"`

	Streams []struct {
		InTopic  string `yaml:"in_topic"`
		InGroup  string `yaml:"in_group"`
		OutTopic string `yaml:"out_topic"`
		Handler  string `yaml:"handler"`
	} `yaml:"streams"`
}
