package website

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"git.handmade.network/hmn/hmn/src/config"
	"git.handmade.network/hmn/hmn/src/db"
	"git.handmade.network/hmn/hmn/src/email"
	"git.handmade.network/hmn/hmn/src/hmnurl"
	"git.handmade.network/hmn/hmn/src/logging"
	"git.handmade.network/hmn/hmn/src/models"
	"git.handmade.network/hmn/hmn/src/oops"
	"git.handmade.network/hmn/hmn/src/templates"
	"github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"
)

type ManageSubscriptionTemplateData struct {
	templates.BaseData
	SubscribeUrl          string
	CancelSubscriptionUrl string
}

func ManageSubscription(c *RequestContext) ResponseData {
	var res ResponseData
	res.MustWriteTemplate("manage_subscription.html", ManageSubscriptionTemplateData{
		BaseData:              getBaseData(c, "Manage Subscription", nil),
		SubscribeUrl:          hmnurl.BuildSubscribe(),
		CancelSubscriptionUrl: hmnurl.BuildCancelSubscription(),
	}, c.Perf)
	return res
}

func Subscribe(c *RequestContext) ResponseData {
	sc := stripe.NewClient(config.Config.Stripe.SecretKey)

	params := &stripe.CheckoutSessionCreateParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL:        stripe.String(hmnurl.BuildManageSubscription()),
		CancelURL:         stripe.String(hmnurl.BuildManageSubscription()),
		ClientReferenceID: stripe.String(strconv.Itoa(c.CurrentUser.ID)),
		LineItems: []*stripe.CheckoutSessionCreateLineItemParams{
			{
				Price:    stripe.String(config.Config.Stripe.PriceID),
				Quantity: stripe.Int64(1),
			},
		},
		CustomerEmail: stripe.String(c.CurrentUser.Email),
	}

	if c.CurrentUser.StripeCustomerID != nil {
		params.Customer = stripe.String(*c.CurrentUser.StripeCustomerID)
		params.CustomerEmail = nil
	}

	s, err := sc.V1CheckoutSessions.Create(c, params)
	if err != nil {
		return c.ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to create checkout session"))
	}

	return c.Redirect(s.URL, http.StatusSeeOther)
}

func CancelSubscription(c *RequestContext) ResponseData {
	if c.CurrentUser.StripeCustomerID == nil {
		return c.Redirect(hmnurl.BuildManageSubscription(), http.StatusSeeOther)
	}

	sc := stripe.NewClient(config.Config.Stripe.SecretKey)

	params := &stripe.BillingPortalSessionCreateParams{
		Customer:  stripe.String(*c.CurrentUser.StripeCustomerID),
		ReturnURL: stripe.String(hmnurl.BuildManageSubscription()),
	}
	ps, err := sc.V1BillingPortalSessions.Create(c, params)
	if err != nil {
		return c.ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to create portal session"))
	}

	return c.Redirect(ps.URL, http.StatusSeeOther)
}

func StripeWebhook(c *RequestContext) ResponseData {
	const MaxBodyBytes = int64(65536)
	payload, err := io.ReadAll(io.LimitReader(c.Req.Body, MaxBodyBytes))
	if err != nil {
		return ResponseData{StatusCode: http.StatusBadRequest}
	}

	event, err := webhook.ConstructEventWithOptions(payload, c.Req.Header.Get("Stripe-Signature"), config.Config.Stripe.WebhookSecret, webhook.ConstructEventOptions{
		IgnoreAPIVersionMismatch: true,
	})
	if err != nil {
		logging.Error().Err(err).Msg("failed to verify Stripe webhook signature")
		return ResponseData{StatusCode: http.StatusBadRequest}
	}

	logging.Info().Str("type", string(event.Type)).Msg("received Stripe webhook")

	switch event.Type {
	case "checkout.session.completed":
		var session stripe.CheckoutSession
		err := json.Unmarshal(event.Data.Raw, &session)
		if err != nil {
			return ResponseData{StatusCode: http.StatusBadRequest}
		}

		if session.ClientReferenceID == "" {
			logging.Error().Msg("checkout.session.completed missing client_reference_id")
			break
		}

		userID, err := strconv.Atoi(session.ClientReferenceID)
		if err != nil {
			logging.Error().Err(err).Str("client_reference_id", session.ClientReferenceID).Msg("invalid client_reference_id")
			break
		}

		var renewalDate *time.Time
		sc := stripe.NewClient(config.Config.Stripe.SecretKey)
		// In v84, CurrentPeriodEnd is on SubscriptionItem, not Subscription.
		// We need to fetch the expanded subscription or just get the items.
		sub, err := sc.V1Subscriptions.Retrieve(c, session.Subscription.ID, &stripe.SubscriptionRetrieveParams{
			Expand: []*string{stripe.String("items")},
		})
		if err != nil {
			logging.Error().Err(err).Str("subID", session.Subscription.ID).Msg("failed to fetch subscription for renewal date")
		} else if sub.Items != nil && len(sub.Items.Data) > 0 {
			rd := time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0)
			renewalDate = &rd
		}

		_, err = c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = true, stripe_customer_id = $1, stripe_subscription_id = $2, subscription_status = 'active', current_period_end = $4 WHERE id = $3",
			session.Customer.ID, session.Subscription.ID, userID, renewalDate)
		if err != nil {
			logging.Error().Err(err).Int("userID", userID).Msg("failed to update user subscription status")
		} else {
			logging.Info().Int("userID", userID).Msg("user subscription activated")

			// Send thank you email
			user, err := db.QueryOne[models.User](c, c.Conn, "SELECT $columns FROM hmn_user WHERE id = $1", userID)
			if err != nil {
				logging.Error().Err(err).Int("userID", userID).Msg("failed to fetch user for thank you email")
			} else {
				amountStr := ""
				if session.AmountTotal > 0 {
					currency := strings.ToUpper(string(session.Currency))
					symbol := "$"
					if currency != "USD" {
						symbol = currency + " "
					}
					amountStr = fmt.Sprintf("%s%.2f", symbol, float64(session.AmountTotal)/100.0)
				}

				err = email.SendThankYouEmail(user.Email, user.BestName(), renewalDate, amountStr, c.Perf)
				if err != nil {
					logging.Error().Err(err).Int("userID", userID).Msg("failed to send thank you email")
				}
			}
		}

	case "customer.subscription.updated":
		var sub stripe.Subscription
		err := json.Unmarshal(event.Data.Raw, &sub)
		if err != nil {
			return ResponseData{StatusCode: http.StatusBadRequest}
		}

		var currentPeriodEnd *time.Time
		if sub.Items != nil && len(sub.Items.Data) > 0 {
			t := time.Unix(sub.Items.Data[0].CurrentPeriodEnd, 0)
			currentPeriodEnd = &t
		}

		_, err = c.Conn.Exec(c, `
			UPDATE hmn_user 
			SET 
				subscription_status = $1, 
				cancel_at_period_end = $2,
				is_subscribed = ($1 = 'active' OR $1 = 'trialing'),
				current_period_end = $4
			WHERE stripe_customer_id = $3
		`, sub.Status, sub.CancelAtPeriodEnd, sub.Customer.ID, currentPeriodEnd)
		if err != nil {
			logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to update user subscription from webhook")
		}

		if sub.CancelAtPeriodEnd {
			user, err := db.QueryOne[models.User](c, c.Conn, "SELECT $columns FROM hmn_user WHERE stripe_customer_id = $1", sub.Customer.ID)
			if err != nil {
				logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to fetch user for cancellation email")
			} else {
				var expirationDate *time.Time
				if sub.CancelAt > 0 {
					t := time.Unix(sub.CancelAt, 0)
					expirationDate = &t
				}
				err = email.SendSubscriptionCancelledEmail(user.Email, user.BestName(), expirationDate, c.Perf)
				if err != nil {
					logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to send cancellation initiation email")
				}
			}
		}

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		err := json.Unmarshal(event.Data.Raw, &sub)
		if err != nil {
			return ResponseData{StatusCode: http.StatusBadRequest}
		}

		_, err = c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = false, stripe_subscription_id = NULL, subscription_status = 'canceled' WHERE stripe_customer_id = $1", sub.Customer.ID)
		if err != nil {
			logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to handle subscription deletion")
		} else {
			logging.Info().Str("customerID", sub.Customer.ID).Msg("user subscription deactivated")

			// Only send cancellation email here if it wasn't a "cancel at period end" (which already sent an email)
			if !sub.CancelAtPeriodEnd {
				user, err := db.QueryOne[models.User](c, c.Conn, "SELECT $columns FROM hmn_user WHERE stripe_customer_id = $1", sub.Customer.ID)
				if err != nil {
					logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to fetch user for cancellation email")
				} else {
					err = email.SendSubscriptionCancelledEmail(user.Email, user.BestName(), nil, c.Perf)
					if err != nil {
						logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to send cancellation email")
					}
				}
			}
		}

	case "invoice.paid":
		var inv stripe.Invoice
		err := json.Unmarshal(event.Data.Raw, &inv)
		if err != nil {
			return ResponseData{StatusCode: http.StatusBadRequest}
		}

		if inv.Customer == nil {
			break
		}

		user, err := db.QueryOne[models.User](c, c.Conn, "SELECT $columns FROM hmn_user WHERE stripe_customer_id = $1", inv.Customer.ID)
		if err != nil {
			logging.Error().Err(err).Str("customerID", inv.Customer.ID).Msg("failed to fetch user for invoice.paid")
			break
		}

		// Record payment
		var methodType, last4, brand *string
		var feeCents, netCents *int

		sc := stripe.NewClient(config.Config.Stripe.SecretKey)
		params := &stripe.InvoicePaymentListParams{
			Invoice: stripe.String(inv.ID),
		}
		params.AddExpand("data.payment.charge.balance_transaction")
		params.AddExpand("data.payment.payment_intent.latest_charge")
		var targetCharge *stripe.Charge
		sc.V1InvoicePayments.List(c, params)(func(ip *stripe.InvoicePayment, err error) bool {
			if err != nil {
				return false
			}
			if ip.Payment != nil {
				if ip.Payment.Charge != nil {
					targetCharge = ip.Payment.Charge
					return false
				}
				if ip.Payment.PaymentIntent != nil && ip.Payment.PaymentIntent.LatestCharge != nil {
					targetCharge = ip.Payment.PaymentIntent.LatestCharge
					return false
				}
			}
			return true
		})

		if targetCharge != nil {
			if targetCharge.PaymentMethodDetails != nil {
				mt := string(targetCharge.PaymentMethodDetails.Type)
				methodType = &mt
				if targetCharge.PaymentMethodDetails.Card != nil {
					l4 := targetCharge.PaymentMethodDetails.Card.Last4
					last4 = &l4
					b := string(targetCharge.PaymentMethodDetails.Card.Brand)
					brand = &b
				} else if targetCharge.PaymentMethodDetails.USBankAccount != nil {
					l4 := targetCharge.PaymentMethodDetails.USBankAccount.Last4
					last4 = &l4
					b := targetCharge.PaymentMethodDetails.USBankAccount.BankName
					brand = &b
				}
			}

			// If balance transaction wasn't expanded (e.g. PI case), fetch it
			if targetCharge.BalanceTransaction != nil {
				fc := int(targetCharge.BalanceTransaction.Fee)
				feeCents = &fc
				nc := int(targetCharge.BalanceTransaction.Net)
				netCents = &nc
			} else {
				// Re-retrieve charge with expansion to get fee info
				fullCharge, err := sc.V1Charges.Retrieve(c, targetCharge.ID, &stripe.ChargeRetrieveParams{
					Expand: []*string{stripe.String("balance_transaction")},
				})
				if err == nil && fullCharge.BalanceTransaction != nil {
					fc := int(fullCharge.BalanceTransaction.Fee)
					feeCents = &fc
					nc := int(fullCharge.BalanceTransaction.Net)
					netCents = &nc
				}
			}
		}

		_, err = c.Conn.Exec(c, `
			INSERT INTO user_payment (user_id, stripe_invoice_id, amount_cents, currency, payment_method_type, card_last4, card_brand, paid_at, stripe_fee_cents, net_amount_cents)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (stripe_invoice_id) DO UPDATE SET 
				payment_method_type = EXCLUDED.payment_method_type,
				card_last4 = EXCLUDED.card_last4,
				card_brand = EXCLUDED.card_brand,
				stripe_fee_cents = EXCLUDED.stripe_fee_cents,
				net_amount_cents = EXCLUDED.net_amount_cents
		`, user.ID, inv.ID, inv.AmountPaid, string(inv.Currency), methodType, last4, brand, time.Unix(inv.StatusTransitions.PaidAt, 0), feeCents, netCents)
		if err != nil {
			logging.Error().Err(err).Int("userID", user.ID).Msg("failed to record user payment")
		}
	}

	var res ResponseData
	res.StatusCode = http.StatusOK
	return res
}
