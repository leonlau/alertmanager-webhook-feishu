package rotate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type MentionRotator struct {
	baseDate  time.Time
	cycleDays int
	openIDs   []string
}

// 接受 "Nw"、"Nd" 或 "NwNd"（如 "2w3d"）
var durationRE = regexp.MustCompile(`^(?:([0-9]+)w)?(?:([0-9]+)d)?$`)

// parseDuration 把 "NwNd" 形式转成总天数。空串视为无效（返回 0 + error）。
func parseDuration(durationStr string) (int, error) {
	durationStr = strings.TrimSpace(durationStr)
	if durationStr == "" {
		return 0, fmt.Errorf("empty rotation duration")
	}
	matches := durationRE.FindStringSubmatch(durationStr)
	if matches == nil {
		return 0, fmt.Errorf("not a valid duration string: %q", durationStr)
	}
	weeks, _ := strconv.Atoi(matches[1]) // 空匹配 → 0
	days, _ := strconv.Atoi(matches[2])
	total := weeks*7 + days
	if total <= 0 {
		return 0, fmt.Errorf("rotate duration at least 1: %v", total)
	}
	return total, nil
}

func New(rotationStr string, openIDs []string) (*MentionRotator, error) {
	parts := strings.Split(rotationStr, ":")
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid rotation string: %v", rotationStr)
	}
	baseDateStr := parts[0]
	rotateDuration := parts[1]

	// parse base date: add timezone (system timezone)
	baseDateStr += time.Now().Format("Z07:00")
	baseDate, err := time.Parse("2006-01-02Z07:00", baseDateStr)
	if err != nil {
		return nil, err
	}

	// parse rotate duration
	days, err := parseDuration(rotateDuration)
	if err != nil {
		return nil, err
	}

	return &MentionRotator{
		baseDate:  baseDate,
		cycleDays: days,
		openIDs:   openIDs,
	}, nil
}

func abs(x int) int {
	if x < 0 {
		return -1 * x
	}
	return x
}

// adjustDays 把相对天数（可负）映射到 [1, cycleDays] 区间。
// 防御性检查：cycleDays <= 0 时回退到不调整（而不是 panic）。
func adjustDays(relativeDays int, cycleDays int) int {
	if cycleDays <= 0 {
		return relativeDays
	}
	if relativeDays < 0 {
		// example: -3 -2 -1 1 2 3 4 5
		// cycle = 2
		// -1 => 3
		return abs(cycleDays - relativeDays)
	}
	// = 0 means at that day
	return relativeDays + 1
}

func (r MentionRotator) Rotate(t time.Time) []string {
	if len(r.openIDs) <= 1 {
		return r.openIDs
	}
	// 把不应触发的内部错误降级为兜底返回，避免 panic 杀进程
	defer func() {
		if rec := recover(); rec != nil {
			logrus.Warnf("mention rotator recovered from panic: %v", rec)
		}
	}()

	days := int(t.Sub(r.baseDate) / time.Hour / 24)
	days = adjustDays(days, r.cycleDays)
	if days <= 0 || r.cycleDays <= 0 {
		return r.openIDs
	}
	index := (bucketIndexEveryN(days, r.cycleDays) - 1) % len(r.openIDs)
	return []string{r.openIDs[index]}
}

// bucketIndexEveryN 计算 v 落在 [bucketSize, 2*bucketSize, ...] 的哪个桶。
// 防御性：参数非法时返回 1（兜底），不 panic。
func bucketIndexEveryN(v, bucketSize int) int {
	if bucketSize <= 0 || v <= 0 {
		return 1
	}
	if v%bucketSize != 0 {
		return (v - v%bucketSize + bucketSize) / bucketSize
	}
	return v / bucketSize
}