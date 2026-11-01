// Copyright (C) 2026 ZpoolPilot contributors
// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"github.com/bouldinnathan/zpoolpilot/internal/domain"
)

func parseARCStats(data []byte) (domain.ARC, error) {
	values := make(map[string]uint64)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 3 || fields[0] == "name" {
			continue
		}
		if value, ok := unsigned(fields[len(fields)-1]); ok {
			values[fields[0]] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return domain.ARC{}, err
	}
	for _, name := range []string{"size", "c", "c_max", "hits", "misses", "l2_size", "l2_hits", "l2_misses"} {
		if _, present := values[name]; !present {
			return domain.ARC{}, fmt.Errorf("missing or invalid ARC counter %q", name)
		}
	}
	result := domain.ARC{
		SizeBytes:   values["size"],
		TargetBytes: values["c"],
		MaxBytes:    values["c_max"],
		Hits:        values["hits"],
		Misses:      values["misses"],
		L2SizeBytes: values["l2_size"],
		L2Hits:      values["l2_hits"],
		L2Misses:    values["l2_misses"],
	}
	if total := float64(result.Hits) + float64(result.Misses); total > 0 {
		result.HitRatio = float64(result.Hits) / total
	}
	return result, nil
}
