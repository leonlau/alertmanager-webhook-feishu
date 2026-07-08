package cmd

import (
	"fmt"
)

// handleErr 把 err 转成可被 cobra RunE 返回的形式。
// 不再 os.Exit，跳过 defer 的问题交给 cobra 处理（cobra 会在 RunE 返回后
// 打印错误并以非零码退出，但所有 defer 已执行）。
func handleErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("fatal: %w", err)
}
