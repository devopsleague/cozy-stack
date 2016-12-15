package instances

import (
	"net/http"

	"github.com/cozy/cozy-stack/instance"
	"github.com/cozy/cozy-stack/web/jsonapi"
	"github.com/labstack/echo"
)

func createHandler(c echo.Context) error {
	domain := c.QueryParam("Domain")
	locale := c.QueryParam("Locale")
	i, err := instance.Create(domain, locale, nil)
	if err != nil {
		return wrapError(err)
	}
	return jsonapi.Data(c, http.StatusCreated, i, nil)
}

func listHandler(c echo.Context) error {
	is, err := instance.List()
	if err != nil {
		return wrapError(err)
	}

	objs := make([]jsonapi.Object, len(is))
	for i, in := range is {
		objs[i] = in
	}

	return jsonapi.DataList(c, http.StatusOK, objs, nil)
}

func deleteHandler(c echo.Context) error {
	domain := c.Param("domain")
	i, err := instance.Destroy(domain)
	if err != nil {
		return wrapError(err)
	}
	return jsonapi.Data(c, http.StatusOK, i, nil)
}

func wrapError(err error) error {
	switch err {
	case instance.ErrNotFound:
		return jsonapi.NotFound(err)
	case instance.ErrExists:
		return jsonapi.Conflict(err)
	case instance.ErrIllegalDomain:
		return jsonapi.InvalidParameter("domain", err)
	case instance.ErrMissingToken:
		return jsonapi.BadRequest(err)
	case instance.ErrInvalidToken:
		return jsonapi.BadRequest(err)
	}
	return err
}

// Routes sets the routing for the instances service
func Routes(router *echo.Group) {
	router.GET("/", listHandler)
	router.POST("/", createHandler)
	router.DELETE("/:domain", deleteHandler)
}
