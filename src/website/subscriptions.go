package website

import (
	"net/http"

	"git.handmade.network/hmn/hmn/src/hmnurl"
	"git.handmade.network/hmn/hmn/src/oops"
	"git.handmade.network/hmn/hmn/src/templates"
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
	_, err := c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = true WHERE id = $1", c.CurrentUser.ID)
	if err != nil {
		return c.ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to update subscription"))
	}

	res := c.Redirect(hmnurl.BuildManageSubscription(), http.StatusSeeOther)
	res.AddFutureNotice("success", "You have successfully subscribed!")
	return res
}

func CancelSubscription(c *RequestContext) ResponseData {
	_, err := c.Conn.Exec(c, "UPDATE hmn_user SET is_subscribed = false WHERE id = $1", c.CurrentUser.ID)
	if err != nil {
		return c.ErrorResponse(http.StatusInternalServerError, oops.New(err, "failed to cancel subscription"))
	}

	res := c.Redirect(hmnurl.BuildManageSubscription(), http.StatusSeeOther)
	res.AddFutureNotice("success", "Your subscription has been cancelled.")
	return res
}
