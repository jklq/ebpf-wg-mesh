import * as stylex from "@stylexjs/stylex";
import { fieldStyles, TextInput } from "#/components/ui/field";
import { colors, fonts, space } from "#/styles/tokens.stylex";

const styles = stylex.create({
	builderOption: {
		display: "flex",
		minHeight: "2.25rem",
		flex: "1",
		cursor: "pointer",
		alignItems: "center",
		justifyContent: "center",
		borderRightStyle: { default: "solid", ":last-child": "solid" },
		borderRightWidth: { default: "1px", ":last-child": "0px" },
		borderColor: colors.line,
		paddingInline: space.md,
		fontFamily: fonts.condensed,
		fontSize: "0.75rem",
		lineHeight: "calc(1 / 0.75)",
		fontWeight: "700",
		letterSpacing: "0.08em",
		textTransform: "uppercase",
	},
	selectedBuilder: {
		backgroundColor: colors.surfaceHover,
		color: colors.ink,
		boxShadow: `inset 0 -2px 0 ${colors.accent}`,
	},
	idleBuilder: { color: colors.muted },
	radioInput: { pointerEvents: "none", position: "absolute", opacity: "0%" },
});
export function BuilderOption({
	name,
	label,
	checked,
	onSelect,
}: {
	name: string;
	label: string;
	checked: boolean;
	onSelect: () => void;
}) {
	return (
		<label
			{...stylex.props([
				styles.builderOption,
				checked ? styles.selectedBuilder : styles.idleBuilder,
			])}
		>
			<input
				type="radio"
				{...stylex.props(styles.radioInput)}
				name={name}
				checked={checked}
				onChange={onSelect}
			/>
			{label}
		</label>
	);
}
export function RollingNumberField({
	id,
	label,
	value,
	onChange,
}: {
	id: string;
	label: string;
	value: string;
	onChange: (value: string) => void;
}) {
	return (
		<div>
			<label {...stylex.props(fieldStyles.label)} htmlFor={id}>
				{label}
			</label>
			<TextInput
				id={id}
				value={value}
				onChange={(event) => onChange(event.target.value)}
				inputMode="numeric"
			/>
		</div>
	);
}
