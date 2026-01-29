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
	"github.com/stripe/stripe-go/v81"
	stripePortal "github.com/stripe/stripe-go/v81/billingportal/session"
	stripeSession "github.com/stripe/stripe-go/v81/checkout/session"
	"github.com/stripe/stripe-go/v81/subscription"
	"github.com/stripe/stripe-go/v81/webhook"
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
	stripe.Key = config.Config.Stripe.SecretKey

	params := &stripe.CheckoutSessionParams{
		Mode:              stripe.String(string(stripe.CheckoutSessionModeSubscription)),
		SuccessURL:        stripe.String(hmnurl.BuildManageSubscription()),
		CancelURL:         stripe.String(hmnurl.BuildManageSubscription()),
		ClientReferenceID: stripe.String(strconv.Itoa(c.CurrentUser.ID)),
		LineItems: []*stripe.CheckoutSessionLineItemParams{
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

	s, err := stripeSession.New(params)
	if err != nil {
		return c.ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to create checkout session"))
	}

	return c.Redirect(s.URL, http.StatusSeeOther)
}

func CancelSubscription(c *RequestContext) ResponseData {
	if c.CurrentUser.StripeCustomerID == nil {
		return c.Redirect(hmnurl.BuildManageSubscription(), http.StatusSeeOther)
	}

	stripe.Key = config.Config.Stripe.SecretKey

	params := &stripe.BillingPortalSessionParams{
		Customer:  stripe.String(*c.CurrentUser.StripeCustomerID),
		ReturnURL: stripe.String(hmnurl.BuildManageSubscription()),
	}
	ps, err := stripePortal.New(params)
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

		_, err = c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = true, stripe_customer_id = $1, stripe_subscription_id = $2 WHERE id = $3",
			session.Customer.ID, session.Subscription.ID, userID)
		if err != nil {
			logging.Error().Err(err).Int("userID", userID).Msg("failed to update user subscription status")
		} else {
			logging.Info().Int("userID", userID).Msg("user subscription activated")

			// Send thank you email
			user, err := db.QueryOne[models.User](c, c.Conn, "SELECT $columns FROM hmn_user WHERE id = $1", userID)
			if err != nil {
				logging.Error().Err(err).Int("userID", userID).Msg("failed to fetch user for thank you email")
			} else {
				var renewalDate *time.Time
				if session.Subscription != nil {
					stripe.Key = config.Config.Stripe.SecretKey
					sub, err := subscription.Get(session.Subscription.ID, nil)
					if err != nil {
						logging.Error().Err(err).Str("subID", session.Subscription.ID).Msg("failed to fetch subscription from Stripe for thank you email")
					} else {
						rd := time.Unix(sub.CurrentPeriodEnd, 0)
						renewalDate = &rd
					}
				}
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

	case "customer.subscription.deleted":
		var sub stripe.Subscription
		err := json.Unmarshal(event.Data.Raw, &sub)
		if err != nil {
			return ResponseData{StatusCode: http.StatusBadRequest}
		}

		_, err = c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = false, stripe_subscription_id = NULL WHERE stripe_customer_id = $1", sub.Customer.ID)
		if err != nil {
			logging.Error().Err(err).Str("customerID", sub.Customer.ID).Msg("failed to handle subscription deletion")
		} else {
			logging.Info().Str("customerID", sub.Customer.ID).Msg("user subscription deactivated")
		}
	}

	var res ResponseData
	res.StatusCode = http.StatusOK
	return res
}
