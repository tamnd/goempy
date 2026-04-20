package pip

import (
	"fmt"
	"github.com/tamnd/goempy/embed_util"
	"github.com/tamnd/goempy/pip/internal/data"
)

func NewPipLib(name string) (*embed_util.EmbeddedFiles, error) {
	return embed_util.NewEmbeddedFiles(data.Data, fmt.Sprintf("pip-%s", name))
}
