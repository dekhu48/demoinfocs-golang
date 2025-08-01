package fake

import (
	"github.com/golang/geo/r3"
	"github.com/stretchr/testify/mock"

	st "github.com/markus-wa/demoinfocs-golang/v5/pkg/demoinfocs/sendtables"
)

// NewEntityWithProperty creates and returns an entity with a single mocked property.
func NewEntityWithProperty(name string, val st.PropertyValue) *Entity {
	entity := new(Entity)

	prop := new(Property)
	prop.On("Value").Return(val)
	entity.On("Property", name).Return(prop)

	entity.On("PropertyValue", name).Return(val, true)
	entity.On("PropertyValueMust", name).Return(val)

	return entity
}

var _ st.Entity = new(Entity)

// Entity is a mock for of sendtables.Entity.
type Entity struct {
	mock.Mock
}

// ServerClass is a mock-implementation of Entity.ServerClass().
func (e *Entity) ServerClass() st.ServerClass {
	return e.Called().Get(0).(st.ServerClass)
}

// ID is a mock-implementation of Entity.ID().
func (e *Entity) ID() int {
	return e.Called().Int(0)
}

// SerialNum is a mock-implementation of Entity.SerialNum().
func (e *Entity) SerialNum() int {
	return e.Called().Int(0)
}

// Properties is a mock-implementation of Entity.Properties().
func (e *Entity) Properties() []st.Property {
	return e.Called().Get(0).([]st.Property)
}

// Property is a mock-implementation of Entity.Property().
func (e *Entity) Property(name string) st.Property {
	v := e.Called(name).Get(0)
	if v == nil {
		// see https://stackoverflow.com/questions/13476349/check-for-nil-and-nil-interface-in-go
		return nil
	}

	return v.(st.Property)
}

// BindProperty is a mock-implementation of Entity.BindProperty().
func (e *Entity) BindProperty(name string, variable any, valueType st.PropertyValueType) {
	e.Called(name, variable, valueType)
}

// PropertyValue is a mock-implementation of Entity.PropertyValue().
func (e *Entity) PropertyValue(name string) (st.PropertyValue, bool) {
	args := e.Called(name)

	return args.Get(0).(st.PropertyValue), args.Bool(1)
}

// PropertyValueMust is a mock-implementation of Entity.PropertyValueMust().
func (e *Entity) PropertyValueMust(name string) st.PropertyValue {
	args := e.Called(name)

	return args.Get(0).(st.PropertyValue)
}

// Position is a mock-implementation of Entity.Position().
func (e *Entity) Position() r3.Vector {
	return e.Called().Get(0).(r3.Vector)
}

// OnPositionUpdate is a mock-implementation of Entity.OnPositionUpdate().
func (e *Entity) OnPositionUpdate(handler func(pos r3.Vector)) {
	e.Called(handler)
}

// BindPosition is a mock-implementation of Entity.BindPosition().
func (e *Entity) BindPosition(pos *r3.Vector) {
	e.Called(pos)
}

// OnDestroy is a mock-implementation of Entity.OnDestroy().
func (e *Entity) OnDestroy(delegate func()) {
	e.Called(delegate)
}

// Destroy is a mock-implementation of Entity.Destroy().
func (e *Entity) Destroy() {
	e.Called()
}

// OnCreateFinished is a mock-implementation of Entity.OnCreateFinished().
func (e *Entity) OnCreateFinished(delegate func()) {
	e.Called()
}
