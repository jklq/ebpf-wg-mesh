import * as stylex from "@stylexjs/stylex";
import { type ReactNode, useEffect, useRef } from "react";
import { createPortal } from "react-dom";
import { colors, shape, space } from "#/styles/tokens.stylex";

const dialogs: HTMLElement[] = [];
let originalOverflow = "";
const focusableSelector =
	'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function Dialog({
	children,
	styles,
	label,
	onClose,
	closeOnBackdrop = true,
}: {
	children: ReactNode;
	styles?: stylex.StyleXStyles;
	label: string;
	onClose?: () => void;
	closeOnBackdrop?: boolean;
}) {
	const ref = useRef<HTMLDivElement>(null);
	const closeRef = useRef(onClose);
	closeRef.current = onClose;
	const previousFocusRef = useRef(
		typeof document === "undefined"
			? null
			: (document.activeElement as HTMLElement | null),
	);

	useEffect(() => {
		const dialog = ref.current;
		if (!dialog) return;
		const previousFocus = previousFocusRef.current;
		if (dialogs.length === 0) {
			originalOverflow = document.body.style.overflow;
			document.body.style.overflow = "hidden";
		}
		dialogs.push(dialog);
		const focusable = () =>
			Array.from(
				dialog.querySelectorAll<HTMLElement>(focusableSelector),
			).filter(
				(el) =>
					el.tabIndex >= 0 &&
					!el.closest('[hidden], [aria-hidden="true"], [inert]'),
			);
		const focusFirst = () => (focusable()[0] ?? dialog).focus();
		if (!dialog.contains(document.activeElement)) focusFirst();
		const onKeyDown = (event: KeyboardEvent) => {
			if (dialogs.at(-1) !== dialog || event.defaultPrevented) return;
			if (event.key === "Escape") {
				event.preventDefault();
				event.stopPropagation();
				closeRef.current?.();
			}
			if (event.key !== "Tab") return;
			const elements = focusable();
			const first = elements[0],
				last = elements.at(-1);
			if (
				!first ||
				(event.shiftKey &&
					(document.activeElement === first ||
						document.activeElement === dialog))
			) {
				event.preventDefault();
				(last ?? dialog).focus();
			} else if (
				!event.shiftKey &&
				(document.activeElement === last ||
					!dialog.contains(document.activeElement))
			) {
				event.preventDefault();
				first.focus();
			}
		};
		const onFocus = (event: FocusEvent) => {
			if (dialogs.at(-1) === dialog && !dialog.contains(event.target as Node))
				focusFirst();
		};
		document.addEventListener("keydown", onKeyDown);
		document.addEventListener("focusin", onFocus);
		return () => {
			document.removeEventListener("keydown", onKeyDown);
			document.removeEventListener("focusin", onFocus);
			const wasTopmost = dialogs.at(-1) === dialog;
			dialogs.splice(dialogs.indexOf(dialog), 1);
			if (dialogs.length === 0) document.body.style.overflow = originalOverflow;
			if (wasTopmost && previousFocus?.isConnected) previousFocus.focus();
		};
	}, []);

	const overlay = (
		<div
			ref={ref}
			role="dialog"
			aria-modal="true"
			aria-label={label}
			tabIndex={-1}
			{...stylex.props(dialogStyles.overlay, styles)}
			onClick={(event) => {
				if (
					closeOnBackdrop &&
					event.target === event.currentTarget &&
					dialogs.at(-1) === ref.current
				)
					onClose?.();
			}}
			onKeyDown={(event) => {
				if (
					event.key === "Escape" &&
					!event.defaultPrevented &&
					dialogs.at(-1) === ref.current
				) {
					event.preventDefault();
					event.stopPropagation();
					onClose?.();
				}
			}}
		>
			{children}
		</div>
	);
	return typeof document === "undefined"
		? overlay
		: createPortal(overlay, document.body);
}

const fadeIn = stylex.keyframes({ from: { opacity: 0 }, to: { opacity: 1 } });
const slideDown = stylex.keyframes({
	from: { opacity: 0, transform: "translateY(-6px)" },
	to: { opacity: 1, transform: "translateY(0)" },
});
export const dialogStyles = stylex.create({
	overlay: {
		position: "fixed",
		inset: 0,
		zIndex: 100,
		display: "flex",
		alignItems: "flex-start",
		justifyContent: "center",
		overflowY: "auto",
		paddingInline: { default: space.xl, "@media (max-width: 640px)": space.sm },
		paddingBlock: { default: 48, "@media (max-width: 640px)": space.lg },
		backgroundColor: "rgba(15,14,13,0.9)",
		backdropFilter: "blur(2px)",
		animation: `${fadeIn} 120ms ease`,
	},
	card: {
		width: "100%",
		maxWidth: 520,
		maxHeight: "calc(100dvh - 32px)",
		overflowY: "auto",
		borderRadius: shape.card,
		borderWidth: 1,
		borderStyle: "solid",
		borderColor: colors.lineBright,
		backgroundColor: colors.surface,
		animation: `${slideDown} 160ms cubic-bezier(0.22, 1, 0.36, 1)`,
	},
});
