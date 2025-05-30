// Code generated by client-gen. DO NOT EDIT.

package v1

import (
	context "context"

	managementv1 "github.com/loft-sh/api/v4/pkg/apis/management/v1"
	scheme "github.com/loft-sh/api/v4/pkg/clientset/versioned/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	gentype "k8s.io/client-go/gentype"
)

// LicenseTokensGetter has a method to return a LicenseTokenInterface.
// A group's client should implement this interface.
type LicenseTokensGetter interface {
	LicenseTokens() LicenseTokenInterface
}

// LicenseTokenInterface has methods to work with LicenseToken resources.
type LicenseTokenInterface interface {
	Create(ctx context.Context, licenseToken *managementv1.LicenseToken, opts metav1.CreateOptions) (*managementv1.LicenseToken, error)
	Update(ctx context.Context, licenseToken *managementv1.LicenseToken, opts metav1.UpdateOptions) (*managementv1.LicenseToken, error)
	// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
	UpdateStatus(ctx context.Context, licenseToken *managementv1.LicenseToken, opts metav1.UpdateOptions) (*managementv1.LicenseToken, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	DeleteCollection(ctx context.Context, opts metav1.DeleteOptions, listOpts metav1.ListOptions) error
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*managementv1.LicenseToken, error)
	List(ctx context.Context, opts metav1.ListOptions) (*managementv1.LicenseTokenList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (result *managementv1.LicenseToken, err error)
	LicenseTokenExpansion
}

// licenseTokens implements LicenseTokenInterface
type licenseTokens struct {
	*gentype.ClientWithList[*managementv1.LicenseToken, *managementv1.LicenseTokenList]
}

// newLicenseTokens returns a LicenseTokens
func newLicenseTokens(c *ManagementV1Client) *licenseTokens {
	return &licenseTokens{
		gentype.NewClientWithList[*managementv1.LicenseToken, *managementv1.LicenseTokenList](
			"licensetokens",
			c.RESTClient(),
			scheme.ParameterCodec,
			"",
			func() *managementv1.LicenseToken { return &managementv1.LicenseToken{} },
			func() *managementv1.LicenseTokenList { return &managementv1.LicenseTokenList{} },
		),
	}
}
