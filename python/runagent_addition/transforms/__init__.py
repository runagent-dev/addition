from __future__ import annotations

from typing import Any, Callable, Dict, Optional

from .gmail import gmail_transforms
from .google_calendar import google_calendar_transforms

TransformFunction = Callable[[Dict[str, Any]], Dict[str, Any]]

_registry: Dict[str, TransformFunction] = {
    **gmail_transforms,
    **google_calendar_transforms,
}


def get_transform(transform_id: str) -> Optional[TransformFunction]:
    """Return the transform for *transform_id*, or None if not registered.

    Lookup is case-sensitive — toolkit YAML references must match exactly.
    """
    return _registry.get(transform_id)
