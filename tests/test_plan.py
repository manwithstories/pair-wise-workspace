from etl.schema.plan import resolve_types


def test_numeric_convergence_ladder():
    assert resolve_types(["int", "decimal"]) == ("decimal", False)
    assert resolve_types(["float", "int"]) == ("float", False)
    assert resolve_types(["int", "int", "int"]) == ("int", False)
    assert resolve_types(["decimal", "float", "int"]) == ("decimal", False)


def test_same_type_converges():
    assert resolve_types(["datetime", "datetime"]) == ("datetime", False)
    assert resolve_types(["string"]) == ("string", False)


def test_cross_family_conflicts():
    assert resolve_types(["datetime", "int"]) == (None, True)
    assert resolve_types(["datetime", "string"]) == (None, True)
    assert resolve_types(["string", "int"]) == (None, True)
