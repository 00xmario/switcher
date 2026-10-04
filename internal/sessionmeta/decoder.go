package sessionmeta

import (
	"context"
	"encoding/json"
)

func decodeMetadata(ctx context.Context, data []byte, dst any) bool {
	return ctx.Err() == nil && json.Unmarshal(data, dst) == nil
}
