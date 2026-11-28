package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yetone/magpie/internal/cloud/payment"
	"github.com/yetone/magpie/internal/cloud/wallet"
	"github.com/yetone/magpie/internal/gateway"
)

func paymentFromEnv(w *wallet.Service, pool payment.Pool) (*payment.Service, error) {
	alipayOn := strings.TrimSpace(os.Getenv("ALIPAY_APP_ID")) != ""
	wechatOn := strings.TrimSpace(os.Getenv("WECHAT_APP_ID")) != ""
	if !alipayOn && !wechatOn {
		return nil, nil
	}
	presets, err := rechargePresets()
	if err != nil {
		return nil, err
	}
	cfg := payment.Config{Presets: presets}
	cfg.Store, err = payment.NewPostgresStore(pool)
	if err != nil {
		return nil, err
	}
	cfg.Creditor = payment.CreditorFunc(func(ctx context.Context, userID uuid.UUID, amount int64, key, description string) error {
		_, err := w.Credit(ctx, userID, amount, key, description)
		return err
	})
	public := strings.TrimRight(gateway.PublicURL(), "/")
	if alipayOn {
		cfg.Alipay, err = payment.NewAlipay(payment.AlipayConfig{
			AppID: os.Getenv("ALIPAY_APP_ID"), PrivateKeyPEM: pemEnv("ALIPAY_PRIVATE_KEY"), AlipayPublicKeyPEM: pemEnv("ALIPAY_PUBLIC_KEY"),
			SellerID: os.Getenv("ALIPAY_SELLER_ID"), GatewayURL: os.Getenv("ALIPAY_GATEWAY_URL"),
			NotifyURL: public + "/api/payments/alipay/notify", ReturnURL: public + "/?view=wallet",
		})
		if err != nil {
			return nil, fmt.Errorf("configure Alipay: %w", err)
		}
	}
	if wechatOn {
		cfg.WeChat, err = payment.NewWeChat(payment.WeChatConfig{
			AppID: os.Getenv("WECHAT_APP_ID"), MchID: os.Getenv("WECHAT_MCH_ID"), MerchantSerialNo: os.Getenv("WECHAT_MERCHANT_SERIAL"),
			PrivateKeyPEM: pemEnv("WECHAT_PRIVATE_KEY"), APIv3Key: os.Getenv("WECHAT_API_V3_KEY"),
			NotifyURL: public + "/api/payments/wechat/notify", PlatformCertificates: certificateBlocks(pemEnv("WECHAT_PLATFORM_CERTIFICATES")),
		})
		if err != nil {
			return nil, fmt.Errorf("configure WeChat Pay: %w", err)
		}
	}
	return payment.New(cfg)
}

func pemEnv(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(os.Getenv(name)), `\n`, "\n")
}

func certificateBlocks(raw string) []string {
	const end = "-----END CERTIFICATE-----"
	var out []string
	for _, part := range strings.Split(raw, end) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-----BEGIN CERTIFICATE-----") {
			out = append(out, part+"\n"+end+"\n")
		}
	}
	return out
}

func rechargePresets() ([]payment.Preset, error) {
	raw := strings.TrimSpace(os.Getenv("MAGPIE_RECHARGE_PRESETS"))
	if raw == "" {
		raw = "10,20,50,100,200,500"
	}
	var out []payment.Preset
	for item := range strings.SplitSeq(raw, ",") {
		item = strings.TrimSpace(item)
		amount, err := payment.FenFromYuan(item)
		if err != nil {
			return nil, fmt.Errorf("MAGPIE_RECHARGE_PRESETS: %w", err)
		}
		out = append(out, payment.Preset{ID: "cny-" + strings.ReplaceAll(item, ".", "-"), AmountFen: amount, Label: "magpie recharge " + amount.YuanString() + " CNY"})
	}
	return out, nil
}

func (r *cloudRuntime) keepCloudJobs(ctx context.Context) {
	defer r.jobs.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	_ = r.reconcileCloudHolds(ctx)
	_ = r.store.CleanupAuthAttempts(ctx, time.Now().Add(-2*time.Hour))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if r.payment != nil {
				if _, err := r.payment.SettlePendingCredits(ctx, 100); err != nil && !errors.Is(err, context.Canceled) {
					// The next tick replays the durable pending set. Callback
					// bodies and merchant secrets are never logged here.
					continue
				}
			}
			_ = r.reconcileCloudHolds(ctx)
			_ = r.store.CleanupAuthAttempts(ctx, time.Now().Add(-2*time.Hour))
		}
	}
}
