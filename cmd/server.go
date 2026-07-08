package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xujiahua/alertmanager-webhook-feishu/config"
	"github.com/xujiahua/alertmanager-webhook-feishu/feishu"
	"github.com/xujiahua/alertmanager-webhook-feishu/server"
)

var port int
var splitByStatus bool
var cfgFile string
var logFormat string

// 优雅关闭最长等待时间（覆盖正常重试退避 6s）
const shutdownTimeout = 30 * time.Second

// serverCmd represents the server command
var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "start webhook server",
	RunE: func(cmd *cobra.Command, args []string) error {
		// JSONFormatter 便于 Loki/ELK 等聚合系统解析；
		// text 模式更易人眼阅读（适合本地开发）。
		if logFormat == "text" {
			logrus.SetFormatter(&logrus.TextFormatter{
				FullTimestamp: true,
			})
		} else {
			logrus.SetFormatter(&logrus.JSONFormatter{
				TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
			})
		}

		if verbose {
			logrus.SetReportCaller(true)
			logrus.SetLevel(logrus.DebugLevel)
		}

		// cfg
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return handleErr(err)
		}

		// bots
		bots := make(map[string]feishu.IBot)
		for group, botCfg := range cfg.Bots {
			bot, err := feishu.New(botCfg)
			if err != nil {
				return fmt.Errorf("create bot %q: %w", group, err)
			}
			bots[group] = bot
			logrus.Infof("bot %s created", group)
		}

		// signal handler 必须在 Start 之前注册，否则端口冲突时
		// ListenAndServe 失败 → os.Exit，进程死得比信号 handler 还快。
		// signalChan 只接收关闭信号（SIGHUP 由 reloadChan 单独处理，
		// 避免 Go runtime 同时投递到两个 channel 导致 select 随机选中关闭分支）。
		signalChan := make(chan os.Signal, 1)
		signal.Notify(
			signalChan,
			syscall.SIGINT,  // kill -SIGINT XXXX or Ctrl+c
			syscall.SIGQUIT, // kill -SIGQUIT XXXX
		)
		// SIGHUP 用于 reload（暂未实现）
		reloadChan := make(chan os.Signal, 1)
		signal.Notify(reloadChan, syscall.SIGHUP)
		defer signal.Stop(signalChan)
		defer signal.Stop(reloadChan)

		// start server in goroutine；启动错误通过 channel 传回主流程
		s := server.New(bots, splitByStatus)
		startErr := make(chan error, 1)
		go func() {
			address := fmt.Sprintf("0.0.0.0:%d", port)
			startErr <- s.Start(address)
		}()

		// 主循环：要么 Start 报错，要么收到关闭信号
		for {
			select {
			case err := <-startErr:
				if err != nil {
					return fmt.Errorf("server failed: %w", err)
				}
				return nil

			case sig := <-signalChan:
				// SIGHUP 已通过 reloadChan 单独处理，到达这里的是 SIGINT/SIGQUIT
				logrus.Infof("received signal %s, shutting down (timeout=%s)", sig, shutdownTimeout)
				ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
				defer cancel()
				if err := s.Shutdown(ctx); err != nil {
					logrus.Errorf("graceful shutdown failed: %v", err)
				}
				return nil

			case <-reloadChan:
				logrus.Info("received SIGHUP, reload not yet implemented")
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)

	serverCmd.Flags().IntVarP(&port, "port", "p", 8000, "server port")
	serverCmd.Flags().BoolVarP(&splitByStatus, "split", "", false, "if enabled, sending firing and resolved alerts in two notifications")
	serverCmd.Flags().StringVarP(&cfgFile, "config", "c", "", "config file for bot webhook")
	serverCmd.Flags().StringVar(&logFormat, "log-format", "json", "log format: json or text")
}
