package components

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
)

// Confirmed reports whether the POST carries the exact name the operator was
// asked to type. Every delete that destroys data uses it, so the check is in
// one place: the browser only disables a button, and a disabled button is not
// a guard.
//
// An empty expectation is never satisfiable. A row with no name would
// otherwise be removable by sending nothing at all, which is the opposite of
// what confirm-by-typing is for.
func Confirmed(c echo.Context, want string) bool {
	if want == "" {
		return false
	}
	return strings.TrimSpace(c.FormValue("confirm")) == want
}

// RequireConfirm is Confirmed as a guard clause: nil to carry on, an error to
// return as-is. The message names what should have been typed, because the
// only people who see it are talking to the API by hand.
func RequireConfirm(c echo.Context, want string) error {
	if Confirmed(c, want) {
		return nil
	}
	return echo.NewHTTPError(http.StatusBadRequest, "type "+want+" to confirm")
}
